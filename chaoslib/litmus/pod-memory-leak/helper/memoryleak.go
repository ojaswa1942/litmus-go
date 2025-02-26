package helper

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	clients "github.com/litmuschaos/litmus-go/pkg/clients"
	"github.com/litmuschaos/litmus-go/pkg/events"
	experimentEnv "github.com/litmuschaos/litmus-go/pkg/generic/pod-memory-leak/environment"
	experimentTypes "github.com/litmuschaos/litmus-go/pkg/generic/pod-memory-leak/types"
	"github.com/litmuschaos/litmus-go/pkg/log"
	"github.com/litmuschaos/litmus-go/pkg/result"
	"github.com/litmuschaos/litmus-go/pkg/types"
	"github.com/litmuschaos/litmus-go/pkg/utils/common"
	"github.com/pkg/errors"
	clientTypes "k8s.io/apimachinery/pkg/types"
)

var (
	err                error
	abort, injectAbort chan os.Signal
)

// Helper injects the dns chaos
func Helper(clients clients.ClientSets) {

	experimentsDetails := experimentTypes.ExperimentDetails{}
	eventsDetails := types.EventDetails{}
	chaosDetails := types.ChaosDetails{}
	resultDetails := types.ResultDetails{}

	// abort channel is used to transmit signal notifications.
	abort = make(chan os.Signal, 1)
	// abort channel is used to transmit signal notifications.
	injectAbort = make(chan os.Signal, 1)

	// Catch and relay certain signal(s) to abort channel.
	signal.Notify(abort, os.Interrupt, syscall.SIGTERM)

	//Fetching all the ENV passed for the helper pod
	log.Info("[PreReq]: Getting the ENV variables")
	getENV(&experimentsDetails)

	// Initialise the chaos attributes
	experimentEnv.InitialiseChaosVariables(&chaosDetails, &experimentsDetails)

	// Initialise Chaos Result Parameters
	types.SetResultAttributes(&resultDetails, chaosDetails)

	// Set the chaos result uid
	result.SetResultUID(&resultDetails, clients, &chaosDetails)

	if err := preparePodMemoryLeak(&experimentsDetails, clients, &eventsDetails, &chaosDetails, &resultDetails); err != nil {
		log.Fatalf("helper pod failed, err: %v", err)
	}

}

// returns stress command
func prepareStressor(experimentDetails *experimentTypes.ExperimentDetails) string {

	stressCmd := []string{
		"memory-leak",
		strconv.Itoa(experimentDetails.TargetMemoryConsumption),
		strconv.Itoa(experimentDetails.ChaosDuration),
	}
	return strings.Join(stressCmd, " ")
}

//preparePodMemoryLeak contains the preparation steps before chaos injection
func preparePodMemoryLeak(experimentsDetails *experimentTypes.ExperimentDetails, clients clients.ClientSets, eventsDetails *types.EventDetails, chaosDetails *types.ChaosDetails, resultDetails *types.ResultDetails) error {

	containerID, err := getContainerID(experimentsDetails, clients)
	if err != nil {
		return err
	}
	// extract out the pid of the target container
	pid, err := common.GetPID(experimentsDetails.ContainerRuntime, containerID, experimentsDetails.SocketPath)
	if err != nil {
		return err
	}

	// record the event inside chaosengine
	if experimentsDetails.EngineName != "" {
		msg := "Injecting " + experimentsDetails.ExperimentName + " chaos on application pod"
		types.SetEngineEventAttributes(eventsDetails, types.ChaosInject, msg, "Normal", chaosDetails)
		events.GenerateEvents(eventsDetails, clients, chaosDetails, "ChaosEngine")
	}

	// prepare memory-leak
	// stressCommand := "pause nsutil -t " + strconv.Itoa(pid) + " -p -- " + prepareStressor(experimentsDetails)
	stressCommand := "sudo nsutil -t " + strconv.Itoa(pid) + " -p -- " + prepareStressor(experimentsDetails)
	cmd := exec.Command("/bin/bash", "-c", stressCommand)
	log.Info(cmd.String())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// injecting memory-leak chaos inside target container
	go func() {
		select {
		case <-injectAbort:
			log.Info("[Chaos]: Abort received, skipping chaos injection")
		default:
			err = cmd.Run()
			if err != nil {
				log.Fatalf("memory leak failed : %v", err)
			}
		}
	}()

	if err = result.AnnotateChaosResult(resultDetails.Name, chaosDetails.ChaosNamespace, "injected", "pod", experimentsDetails.TargetPods); err != nil {
		return err
	}

	// additional 10s for custom leak process to gracefully terminate (similar to IO stressors)
	timeChan := time.Tick(time.Duration(experimentsDetails.ChaosDuration+10) * time.Second)
	log.Infof("[Chaos]: Waiting for %vs", experimentsDetails.ChaosDuration)

	// either wait for abort signal or chaos duration
	select {
	case <-abort:
		log.Info("[Chaos]: Killing process started because of terminated signal received")
	case <-timeChan:
		log.Info("[Chaos]: Stopping the experiment, chaos duration over")
	}

	log.Info("Chaos Revert Started")
	// retry thrice for the chaos revert

	retry := 3
	for retry > 0 {
		if cmd.Process == nil {
			log.Infof("cannot kill memory-leak, process not started. Retrying in 1sec...")
		} else {
			log.Infof("killing memory-leak with pid %v", cmd.Process.Pid)
			// kill command
			killTemplate := fmt.Sprintf("sudo kill %d", cmd.Process.Pid)
			kill := exec.Command("/bin/bash", "-c", killTemplate)
			if err = kill.Run(); err != nil {
				log.Errorf("unable to kill memory-leak process cry, err :%v", err)
			} else {
				log.Errorf("memory leak process stopped")
				break
			}
		}
		retry--
		time.Sleep(1 * time.Second)
	}
	if err = result.AnnotateChaosResult(resultDetails.Name, chaosDetails.ChaosNamespace, "reverted", "pod", experimentsDetails.TargetPods); err != nil {
		return err
	}
	log.Info("Chaos Revert Completed")
	return nil
}

//getContainerID extract out the container id of the target container
func getContainerID(experimentDetails *experimentTypes.ExperimentDetails, clients clients.ClientSets) (string, error) {

	var containerID string
	switch experimentDetails.ContainerRuntime {
	case "docker":
		host := "unix://" + experimentDetails.SocketPath
		// deriving the container id of the pause container
		cmd := "sudo docker --host " + host + " ps | grep k8s_POD_" + experimentDetails.TargetPods + "_" + experimentDetails.AppNS + " | awk '{print $1}'"
		out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
		if err != nil {
			log.Error(fmt.Sprintf("[docker]: Failed to run docker ps command: %s", string(out)))
			return "", err
		}
		containerID = strings.TrimSpace(string(out))
	case "containerd", "crio":
		containerID, err = common.GetContainerID(experimentDetails.AppNS, experimentDetails.TargetPods, experimentDetails.TargetContainer, clients)
		if err != nil {
			return containerID, err
		}
	default:
		return "", errors.Errorf("%v container runtime not suported", experimentDetails.ContainerRuntime)
	}
	log.Infof("Container ID: %v", containerID)

	return containerID, nil
}

//getENV fetches all the env variables from the runner pod
func getENV(experimentDetails *experimentTypes.ExperimentDetails) {
	experimentDetails.ExperimentName = common.Getenv("EXPERIMENT_NAME", "")
	experimentDetails.AppNS = common.Getenv("APP_NS", "")
	experimentDetails.TargetContainer = common.Getenv("APP_CONTAINER", "")
	experimentDetails.TargetPods = common.Getenv("APP_POD", "")
	experimentDetails.ChaosDuration, _ = strconv.Atoi(common.Getenv("TOTAL_CHAOS_DURATION", "100"))
	experimentDetails.ChaosNamespace = common.Getenv("CHAOS_NAMESPACE", "litmus")
	experimentDetails.EngineName = common.Getenv("CHAOS_ENGINE", "")
	experimentDetails.ChaosUID = clientTypes.UID(common.Getenv("CHAOS_UID", ""))
	experimentDetails.ChaosPodName = common.Getenv("POD_NAME", "")
	experimentDetails.ContainerRuntime = common.Getenv("CONTAINER_RUNTIME", "")
	experimentDetails.SocketPath = common.Getenv("SOCKET_PATH", "")
	experimentDetails.TargetMemoryConsumption, _ = strconv.Atoi(common.Getenv("TARGET_MEMORY_CONSUMPTION", "500"))
}
