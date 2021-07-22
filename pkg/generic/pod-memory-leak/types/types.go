package types

import (
	corev1 "k8s.io/api/core/v1"
	clientTypes "k8s.io/apimachinery/pkg/types"
)

// ADD THE ATTRIBUTES OF YOUR CHOICE HERE
// FEW MENDATORY ATTRIBUTES ARE ADDED BY DEFAULT

// ExperimentDetails is for collecting all the experiment-related details
type ExperimentDetails struct {
	ExperimentName                string
	EngineName                    string
	ChaosDuration                 int
	RampTime                      int
	ChaosLib                      string
	AppNS                         string
	AppLabel                      string
	AppKind                       string
	ChaosUID                      clientTypes.UID
	InstanceID                    string
	ChaosNamespace                string
	ChaosPodName                  string
	RunID                         string
	Timeout                       int
	Delay                         int
	Annotations                   map[string]string
	ContainerRuntime              string
	ChaosServiceAccount           string
	SocketPath                    string
	TerminationGracePeriodSeconds int
	TargetContainer               string
	ChaosInjectCmd                string
	ChaosKillCmd                  string
	PodsAffectedPerc              int
	TargetPods                    string
	TargetMemoryConsumption       int
	Sequence                      string
	LIBImage                      string
	LIBImagePullPolicy            string
	Resources                     corev1.ResourceRequirements
	ImagePullSecrets              []corev1.LocalObjectReference
}
