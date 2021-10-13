package helper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
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

//list of cgroups in a container
var (
	cgroupSubsystemList = []string{"cpu", "memory", "systemd", "net_cls",
		"net_prio", "freezer", "blkio", "perf_event", "devices", "cpuset",
		"cpuacct", "pids", "hugetlb",
	}
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

	containerID, err := common.GetContainerID(experimentsDetails.AppNS, experimentsDetails.TargetPods, experimentsDetails.TargetContainer, clients)
	if err != nil {
		return err
	}
	// extract out the pid of the target container
	// pid, err := common.GetPID(experimentsDetails.ContainerRuntime, containerID, experimentsDetails.SocketPath)
	pid, err := getPID(experimentsDetails, containerID)
	if err != nil {
		return err
	}

	// record the event inside chaosengine
	if experimentsDetails.EngineName != "" {
		msg := "Injecting " + experimentsDetails.ExperimentName + " chaos on application pod"
		types.SetEngineEventAttributes(eventsDetails, types.ChaosInject, msg, "Normal", chaosDetails)
		events.GenerateEvents(eventsDetails, clients, chaosDetails, "ChaosEngine")
	}

	//get the pid path and check cgroup
	path := pidPath(pid)
	cgroup, err := findValidCgroup(path, containerID)
	if err != nil {
		return errors.Errorf("fail to get cgroup, err: %v", err)
	}

	// load the existing cgroup
	control, err := cgroups.Load(cgroups.V1, cgroups.StaticPath(cgroup))
	if err != nil {
		return errors.Errorf("fail to load the cgroup, err: %v", err)
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

	// add the stress process to the cgroup of target container
	if err = control.Add(cgroups.Process{Pid: cmd.Process.Pid}); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			return errors.Errorf("stressors failed killing %v process, err: %v", cmd.Process.Pid, killErr)
		}
		return errors.Errorf("fail to add the stress process into target container cgroup, err: %v", err)
	}

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

//getPID extract out the PID of the target container
func getPID(experimentDetails *experimentTypes.ExperimentDetails, containerID string) (int, error) {
	var PID int

	switch experimentDetails.ContainerRuntime {
	case "docker":
		host := "unix://" + experimentDetails.SocketPath
		// deriving pid from the inspect out of target container
		out, err := exec.Command("sudo", "docker", "--host", host, "inspect", containerID).CombinedOutput()
		if err != nil {
			log.Error(fmt.Sprintf("[docker]: Failed to run docker inspect: %s", string(out)))
			return 0, err
		}

		// parsing data from the json output of inspect command
		PID, err = parsePIDFromJSON(out, experimentDetails.ContainerRuntime)
		if err != nil {
			log.Error(fmt.Sprintf("[docker]: Failed to parse json from docker inspect output: %s", string(out)))
			return 0, err
		}

	case "containerd", "crio":
		// deriving pid from the inspect out of target container
		endpoint := "unix://" + experimentDetails.SocketPath
		out, err := exec.Command("sudo", "crictl", "-i", endpoint, "-r", endpoint, "inspect", containerID).CombinedOutput()
		if err != nil {
			log.Error(fmt.Sprintf("[cri]: Failed to run crictl: %s", string(out)))
			return 0, err
		}

		// parsing data from the json output of inspect command
		PID, err = parsePIDFromJSON(out, experimentDetails.ContainerRuntime)
		if err != nil {
			log.Errorf(fmt.Sprintf("[cri]: Failed to parse json from crictl output: %s", string(out)))
			return 0, err
		}
	default:
		return 0, errors.Errorf("%v container runtime not suported", experimentDetails.ContainerRuntime)
	}

	log.Info(fmt.Sprintf("[Info]: Container ID=%s has process PID=%d", containerID, PID))

	return PID, nil
}

//pidPath will get the pid path of the container
func pidPath(pid int) cgroups.Path {
	processPath := "/proc/" + strconv.Itoa(pid) + "/cgroup"
	paths, err := parseCgroupFile(processPath)
	if err != nil {
		return getErrorPath(errors.Wrapf(err, "parse cgroup file %s", processPath))
	}
	return getExistingPath(paths, pid, "")
}

//parseCgroupFile will read and verify the cgroup file entry of a container
func parseCgroupFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.Errorf("unable to parse cgroup file: %v", err)
	}
	defer file.Close()
	return parseCgroupFromReader(file)
}

//parseCgroupFromReader will parse the cgroup file from the reader
func parseCgroupFromReader(r io.Reader) (map[string]string, error) {
	var (
		cgroups = make(map[string]string)
		s       = bufio.NewScanner(r)
	)
	for s.Scan() {
		var (
			text  = s.Text()
			parts = strings.SplitN(text, ":", 3)
		)
		if len(parts) < 3 {
			return nil, errors.Errorf("invalid cgroup entry: %q", text)
		}
		for _, subs := range strings.Split(parts[1], ",") {
			if subs != "" {
				cgroups[subs] = parts[2]
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, errors.Errorf("buffer scanner failed: %v", err)
	}

	return cgroups, nil
}

//getExistingPath will be used to get the existing valid cgroup path
func getExistingPath(paths map[string]string, pid int, suffix string) cgroups.Path {
	for n, p := range paths {
		dest, err := getCgroupDestination(pid, n)
		if err != nil {
			return getErrorPath(err)
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil {
			return getErrorPath(err)
		}
		if rel == "." {
			rel = dest
		}
		paths[n] = filepath.Join("/", rel)
	}
	return func(name cgroups.Name) (string, error) {
		root, ok := paths[string(name)]
		if !ok {
			if root, ok = paths[fmt.Sprintf("name=%s", name)]; !ok {
				return "", cgroups.ErrControllerNotActive
			}
		}
		if suffix != "" {
			return filepath.Join(root, suffix), nil
		}
		return root, nil
	}
}

//getErrorPath will give the invalid cgroup path
func getErrorPath(err error) cgroups.Path {
	return func(_ cgroups.Name) (string, error) {
		return "", err
	}
}

//getCgroupDestination will validate the subsystem with the mountpath in container mountinfo file.
func getCgroupDestination(pid int, subsystem string) (string, error) {
	mountinfoPath := fmt.Sprintf("/proc/%d/mountinfo", pid)
	file, err := os.Open(mountinfoPath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	s := bufio.NewScanner(file)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		for _, opt := range strings.Split(fields[len(fields)-1], ",") {
			if opt == subsystem {
				return fields[3], nil
			}
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", errors.Errorf("no destination found for %v ", subsystem)
}

//findValidCgroup will be used to get a valid cgroup path
func findValidCgroup(path cgroups.Path, target string) (string, error) {
	for _, subsystem := range cgroupSubsystemList {
		path, err := path(cgroups.Name(subsystem))
		if err != nil {
			log.Errorf("fail to retrieve the cgroup path, subsystem: %v, target: %v, err: %v", subsystem, target, err)
			continue
		}
		if strings.Contains(path, target) {
			return path, nil
		}
	}
	return "", errors.Errorf("never found valid cgroup for %s", target)
}

//parsePIDFromJSON extract the pid from the json output
func parsePIDFromJSON(j []byte, runtime string) (int, error) {
	var pid int
	switch runtime {
	case "docker":
		// in docker, pid is present inside state.pid attribute of inspect output
		var resp []common.DockerInspectResponse
		if err := json.Unmarshal(j, &resp); err != nil {
			return 0, err
		}
		pid = resp[0].State.PID
	case "containerd":
		var resp common.CrictlInspectResponse
		if err := json.Unmarshal(j, &resp); err != nil {
			return 0, err
		}
		pid = resp.Info.PID

	case "crio":
		var info common.InfoDetails
		if err := json.Unmarshal(j, &info); err != nil {
			return 0, err
		}
		pid = info.PID
		if pid == 0 {
			var resp common.CrictlInspectResponse
			if err := json.Unmarshal(j, &resp); err != nil {
				return 0, err
			}
			pid = resp.Info.PID
		}
	default:
		return 0, errors.Errorf("[cri]: No supported container runtime, runtime: %v", runtime)
	}
	if pid == 0 {
		return 0, errors.Errorf("[cri]: No running target container found, pid: %d", pid)
	}

	return pid, nil
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
