package command

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/fischor/dew/internal/port"
	"github.com/fischor/dew/internal/sshtunnel"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/transport/spdy"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// ClientOptions for a dew client. Value passed as command line arguments and
// values derived from them.
type ClientOptions struct {
	genericclioptions.IOStreams

	Namespace        string
	EnforceNamespace bool

	// Required.
	Service string

	// Required.
	RawPortMappings []string

	PortMappings []port.Mapping

	// The port in the running container that SSH connections are accepted
	// on.
	RemoteSSHPort int

	// The port on the localhost that is used to forward SSH connections to
	// the remote container.
	LocalSSHPort int

	RESTConfig *rest.Config
	ClientSet  *kubernetes.Clientset

	GraceSignal <-chan struct{}
}

var (
	dewImage            = "cr.mycom.com:5000/dew-server"
	dewPodContainerName = "main"
	dewServerCommand    = "dew-server"

	clientLong    = ""
	clientExample = ""
)

// NewTunnelCommand creates a ne tunnel command.
//
// The tunnel command works with a graceful shutdown.
func NewTunnelCommand(f cmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := &ClientOptions{
		IOStreams:    streams,
		Namespace:    corev1.NamespaceDefault,
		LocalSSHPort: 7154, // TODO: grab one randomly
	}

	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Tunnel traffic received on pod to your local machine",
		Long:  clientLong,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, args))
			cmdutil.CheckErr(o.Run(cmd.Context()))
		},
	}

	cmd.Flags().StringSliceVarP(&o.RawPortMappings, "publish", "p", nil, "The TCP ports to pipe. Format <target>:<container>/<proto>")

	return cmd
}

func (o *ClientOptions) Complete(f cmdutil.Factory, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("requires exactly one agument, got %d", len(args))
	}
	o.Service = args[0]

	if len(o.RawPortMappings) == 0 {
		return fmt.Errorf("specify one or more ports to tunnel. Use --tcp 8080:8080 or --udp 8080:9090 for example.")
	}
	var err error
	o.PortMappings, err = port.ParseMappings(o.RawPortMappings)
	if err != nil {
		return err
	}
	o.RemoteSSHPort, err = chooseSSHPort(o.PortMappings)
	if err != nil {
		return err
	}

	o.Namespace, o.EnforceNamespace, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	o.RESTConfig, err = f.ToRESTConfig()
	if err != nil {
		return err
	}
	o.ClientSet, err = f.KubernetesClientSet()
	if err != nil {
		return err
	}
	return nil
}

// dew -p <local>:<container>/<protocol> <service>

// dew <service/pod-name> -p 8080:8080 -p 7070:7070
//
// If there is already a service with that name and a dew annotation,
// we can still try to connect to it.

// Run runs the client command.
//
// ctx is closed when the client presses CTRL+C twice.
func (o *ClientOptions) Run(ctx context.Context) error {
	// dewID is attached to the dew pod as a "dew-id" keyed label.
	// TODO: why needed? Can use the service name instead
	dewID := "dew-" + uuid.New().String()

	// Create config map for openssh.
	// TODO: derive an image such that no configmap is necessary.
	configmapClient := o.ClientSet.CoreV1().ConfigMaps(o.Namespace)
	configmap := getConfigMap(o.Service, dewID, o.RemoteSSHPort)
	log.Printf("Creating configmap \"%s\"...\n", o.Service)
	configmap, err := configmapClient.Create(ctx, configmap, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating configmap: %v", err)
	}
	log.Printf("Created configmap \"%s\".", configmap.GetObjectMeta().GetName())
	defer func() {
		log.Printf("Deleting configmap %s ...", o.Service)
		err := configmapClient.Delete(ctx, o.Service, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("error configmap service: %v\n", err)
		}
	}()

	// Create the service for incoming traffic within the cluster. The
	// services aceppts traffic on all ports that are in mentioned in
	// o.PortMappings[*].ContainerPortNumber using the
	// specied protocol.
	serviceClient := o.ClientSet.CoreV1().Services(o.Namespace)
	svcPorts := servicePorts(o.PortMappings)
	service := getService(o.Service, dewID, svcPorts)
	log.Printf("Creating service \"%s\"...\n", o.Service)
	service, err = serviceClient.Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating service: %v", err)
	}
	log.Printf("Created service \"%s\".", service.GetObjectMeta().GetName())
	defer func() {
		log.Printf("Deleting service %s ...", service.Name)
		err := serviceClient.Delete(ctx, service.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("error deleting service: %v\n", err)
		}
	}()

	// Create the service for incoming traffic within the cluster. The pod
	// exposes all ports that are in mentioned in
	// o.PortMappings[*].ContainerPortNumber using the
	// specied protocol. Additionally it exposes port 2222 (or 2223 if
	// 2222 is used already) for ssh connections.
	dewImage = "ghcr.io/linuxserver/openssh-server"
	ports := append(containerPorts(o.PortMappings), corev1.ContainerPort{
		Name:          "ssh",
		ContainerPort: int32(o.RemoteSSHPort),
	})
	podClient := o.ClientSet.CoreV1().Pods(o.Namespace)
	pod := getPod(o.Service, dewID, ports)
	log.Printf("Creating pod \"%s\"...\n", o.Service)
	pod, err = podClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating pod: %v", err)
	}
	log.Printf("Created pod \"%s\".", service.GetObjectMeta().GetName())
	defer func() {
		// TODO: fails when CTRL+C cancels the ctx
		log.Printf("Deleting pod %s ...", pod.Name)
		err := podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			// TODO: write in error message: "there might be pods leftover
			// use dew cleanup to delete all services and pod for dew (that have a label "dew": "helloworld")"
			// dew cleanup will ask for confirmation before cleaning up
			// use -y to omit.
			log.Printf("error deleting pod: %v\n", err)
		}
	}()

	// Wait for the pod to be ready before setting up a SSH connection.
	watchOptions := metav1.ListOptions{}
	watchOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", o.Service).String()
	watchOptions.ResourceVersion = pod.GetResourceVersion()
	podWatch, err := podClient.Watch(ctx, watchOptions)
	if err != nil {
		return fmt.Errorf("error watching pod: %v", err)
	}
	// TODO: sleep here to assure the pod is ready. Then test if UntilWithoutRetry
	// will also receive events from before it was called.
	wctx, wcancel := watchtools.ContextWithOptionalTimeout(context.Background(), 5*time.Minute)
	_, err = watchtools.UntilWithoutRetry(wctx, podWatch, condPodReady)
	wcancel()
	if err != nil {
		if err == watchtools.ErrWatchClosed {
			return fmt.Errorf("error waiting for pod ready: podWatch has been closed before pod ready event received")
		}
		if err == wait.ErrWaitTimeout {
			return fmt.Errorf("error waiting for pod ready: timed out after %d seconds", 300)
		}
		return fmt.Errorf("error waiting for pod ready: received unknown error \"%f\"", err)
	}
	log.Printf("Pod ready..\n")

	// Do a portforwarding to the opened SSH port.
	req := o.ClientSet.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(o.RESTConfig)
	if err != nil {
		return err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", req.URL())
	pfwdReadyCh := make(chan struct{}) // closed when portforwarding ready?
	pfwdStopCh := make(chan struct{}, 1)
	// Alternatively use these streams for pforward
	// var berr, bout bytes.Buffer
	// buffErr := bufio.NewWriter(&berr)
	// buffOut := bufio.NewWriter(&bout)
	fw, err := portforward.NewOnAddresses(
		dialer,
		[]string{"localhost"},
		[]string{fmt.Sprintf("%d:%d", o.LocalSSHPort, o.RemoteSSHPort)},
		pfwdStopCh,
		pfwdReadyCh,
		o.Out,
		o.ErrOut,
	)
	if err != nil {
		return err
	}
	pfwdDoneCh := make(chan struct{}) // close when portforwarding exits.
	go func() {
		err := fw.ForwardPorts() // blocks
		if err != nil {
			fmt.Printf("error Forwarding SSH port: %v\n", err)
		}
		// If this errors, also everything following will error.
		close(pfwdDoneCh)
	}()

	// TODO: select on readyCh and time.After? Or is it possible to put a
	// timeout in the http.Client.Transport for Dial?
	<-pfwdReadyCh // also gets closed when ForwardPorts errors
	log.Printf("port forwarding to url: %s\n", req.URL().String())
	log.Printf("port forwarding  is ready: forwarding :%d --> pod %d\n", o.LocalSSHPort, o.RemoteSSHPort)

	// log.Println("check now")
	// select {
	// case <-ctx.Done():
	// 	close(pfwdStopCh)
	// 	<-pfwdDoneCh
	// 	return ctx.Err()
	// case <-time.After(120 * time.Second):
	// }
	<-time.After(10 * time.Second)

	// The pod is now ready. Execute the command.
	// Establish ssh connection over the forwarded port.
	sshConfig := &ssh.ClientConfig{
		User: "user",
		Auth: []ssh.AuthMethod{
			ssh.Password("password"),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			// Accept all keys.
			return nil
		},
	}
	sshAddr := fmt.Sprintf("localhost:%d", o.LocalSSHPort)
	sshClient, err := ssh.Dial("tcp", sshAddr, sshConfig)
	if err != nil {
		close(pfwdStopCh)
		<-pfwdDoneCh
		log.Printf("error ssh.Dial:%v\n", err)
		return err
	}
	log.Printf("ssl connection ready 127.0.0.1:%d\n", o.LocalSSHPort)

	var tunnels []*sshtunnel.Tunnel
	for _, m := range o.PortMappings {
		// TODO support remote ips
		// Note that it does not work without the 0.0.0.0 here.
		remote := fmt.Sprintf("0.0.0.0:%d", m.ContainerPortNumber)
		forward := m.TargetAddress()
		t, err := sshtunnel.Setup(sshClient, remote, forward)
		if err != nil {
			// Close all created tunnels.
			for _, t := range tunnels {
				t.Stop()
			}
			for _, t := range tunnels {
				select {
				case <-ctx.Done():
					// Command killed. Leave tunnels left
					// unclosed. Also portforwarding.
					return ctx.Err()
				case <-t.Done():
				}
			}
			// Close portforward.
			close(pfwdStopCh)
			<-pfwdDoneCh
			return fmt.Errorf("failed to setup tunnel from %s to %s: %v", remote, forward, err)
		}
		tunnels = append(tunnels, t)
		log.Printf("tunnel from remove %s to target %s setup\n", remote, forward)
	}

	var wg sync.WaitGroup // wait group for all running tunnels
	for _, t := range tunnels {
		t_ := t
		wg.Add(1)
		go func() {
			// TOOD: this can actually return an error
			// use error group and make an option ContinueOnTunnelError
			// to specify wheter to continue (if more than one tunnel)
			// is left open or not.
			t_.Run()
			wg.Done()
		}()
	}
	go func() {
		// TODO: use graceful here
		<-ctx.Done()
		for _, t := range tunnels {
			t.Stop()
		}
	}()

	// TODO: select on Wait and ctx.Done()
	// - if wait successful, then graceful shutdown happended
	// - if ctx.Done hard kill is requested
	wg.Wait()

	close(pfwdStopCh)
	// Wait for the port forward to end.
	// TODO do not wait in case of ctx.Done
	<-pfwdDoneCh

	return nil
}

func getConfigMap(name string, dewID string, remotePort int) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.dew": dewID,
			},
		},
		Data: map[string]string{
			"ssh-init-gateway.sh": fmt.Sprintf(`#!/bin/bash
# set -e
sed -i 's/#Port 22/Port %d/g' /etc/ssh/sshd_config
sed -i 's/#AllowAgentForwarding yes/AllowAgentForwarding yes/g' /etc/ssh/sshd_config
sed -i 's/AllowTcpForwarding no/AllowTcpForwarding yes/g' /etc/ssh/sshd_config
sed -i 's/GatewayPorts no/GatewayPorts yes/g' /etc/ssh/sshd_config
sed -i 's/X11Forwarding no/X11Forwarding yes/g' /etc/ssh/sshd_config
	    `, remotePort),
		},
	}
}

func getPod(name, dewID string, ports []corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.dew": dewID,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  dewPodContainerName,
				Image: dewImage,
				Ports: ports,
				Env: []corev1.EnvVar{
					{Name: "PASSWORD_ACCESS", Value: "true"},
					{Name: "USER_NAME", Value: "user"},
					{Name: "USER_PASSWORD", Value: "password"},
				},
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "ssh-config",
					MountPath: "/config/custom-cont-init.d/ssh-init-gateway.sh",
					SubPath:   "ssh-init-gateway.sh",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "ssh-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: name,
						},
					},
				},
			}},
		},
	}
}

func getService(name, dewID string, ports []corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.dew": dewID,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"io.github.dew": dewID,
			},
			Ports: ports,
		},
	}

}

func servicePorts(mappings []port.Mapping) []corev1.ServicePort {
	var ports []corev1.ServicePort
	for i, m := range mappings {
		ports = append(ports, corev1.ServicePort{
			Name:       fmt.Sprint(i),
			Port:       int32(m.ContainerPortNumber),
			TargetPort: intstr.FromInt(m.ContainerPortNumber),
			Protocol:   protocolToCoreV1(m.Protocol),
		})
	}
	return ports
}

func containerPorts(mappings []port.Mapping) []corev1.ContainerPort {
	var ports []corev1.ContainerPort
	for _, m := range mappings {
		ports = append(ports, corev1.ContainerPort{
			ContainerPort: int32(m.ContainerPortNumber),
			Protocol:      protocolToCoreV1(m.Protocol),
			// TODO: HostIP?
		})
	}
	return ports
}

func protocolToCoreV1(p port.Protocol) corev1.Protocol {
	if p == port.ProtocolSCTP {
		return corev1.ProtocolSCTP
	}
	if p == port.ProtocolUDP {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

// waitService is a watchtools.ConditionFunc. Waits for the service to have one
// pod attached.
func condPodReady(event watch.Event) (bool, error) {
	pod := event.Object.(*corev1.Pod)
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	return false, nil
}

// chooseSSHPort chooses the port number for the SSH server respecting the ports
// that are used for incoming traffic.
func chooseSSHPort(mm []port.Mapping) (int, error) {
	if !isInUse(mm, 2222) {
		return 2222, nil
	}
	if !isInUse(mm, 22) {
		return 22, nil
	}
	min := 49152
	max := 65535
	for i := min; i <= max; i++ {
		if !isInUse(mm, i) {
			return i, nil
		}
	}
	return 0, fmt.Errorf("Failed to choose a port for the SSH connection - all ports in use")
}

func isInUse(mm []port.Mapping, containerPort int) bool {
	for _, m := range mm {
		if m.ContainerPortNumber == containerPort {
			return true
		}
	}
	return false
}
