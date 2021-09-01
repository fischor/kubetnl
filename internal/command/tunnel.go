package command

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/fischor/dew/internal/interruptcontext"
	"github.com/fischor/dew/internal/port"
	"github.com/fischor/dew/internal/tunnel"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
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
	"k8s.io/kubectl/pkg/util/templates"
)

// TunnelOptions for a dew client. Value passed as command line arguments and
// values derived from them.
type TunnelOptions struct {
	genericclioptions.IOStreams

	Namespace        string
	EnforceNamespace bool

	// Name of the tunnel. This will also be the name of the pod and service.
	Name string

	// Required.
	RawPortMappings []string

	PortMappings []port.Mapping

	// The port in the running container that SSH connections are accepted
	// on.
	RemoteSSHPort int

	ContinueOnTunnelError bool

	// The port on the localhost that is used to forward SSH connections to
	// the remote container.
	LocalSSHPort int

	RESTConfig *rest.Config
	ClientSet  *kubernetes.Clientset

	GraceSignal <-chan struct{}
}

var (
	tunnelLong = templates.LongDesc(`
		Forward TCP traffic from a container running in Kubernetes to another host.
	`)

	tunnelExample = templates.LongDesc(`
		# Tunnel to the local port 8080 from container port 80.
		kubetnl tunnel -p 8080:80 mytunnel

		# Tunnel to port 10.10.10.10:3333 from container port 80.
		kubetnl tunnel -p 10.10.10.10:3333:80 mytunnel

		# Tunnel to local port 8080 from container port 80 and to local port 9090 from container port 90.
		kubetnl tunnel -p 8080:80 -p 9090:90 mytunnel`)
)

var (
	// dewImage            = "cr.mycom.com:5000/dew-server"
	dewImage            = "ghcr.io/linuxserver/openssh-server"
	dewPodContainerName = "main"
	dewServerCommand    = "dew-server"
)

// NewTunnelCommand creates a ne tunnel command.
//
// The tunnel command works with a graceful shutdown.
func NewTunnelCommand(f cmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := &TunnelOptions{
		IOStreams:    streams,
		Namespace:    corev1.NamespaceDefault,
		LocalSSHPort: 7154, // TODO: grab one randomly
	}

	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Tunnel traffic received on pod to your local machine",
		Long:  tunnelLong,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, args))
			ctx, graceCh := interruptcontext.WithGrafulInterrupt(cmd.Context())
			cmdutil.CheckErr(o.Run(ctx, graceCh))
		},
	}

	cmd.Flags().StringSliceVarP(&o.RawPortMappings, "publish", "p", nil, "The TCP ports to pipe. Format <target>:<container>/<proto>")

	return cmd
}

func (o *TunnelOptions) Complete(f cmdutil.Factory, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("requires exactly one agument, got %d", len(args))
	}
	o.Name = args[0]

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
func (o *TunnelOptions) Run(ctx context.Context, graceCh <-chan struct{}) error {
	configmapClient := o.ClientSet.CoreV1().ConfigMaps(o.Namespace)
	configmap := getConfigMap(o.Name, o.RemoteSSHPort)
	log.Printf("Creating configmap \"%s\"...\n", o.Name)
	configmap, err := configmapClient.Create(ctx, configmap, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating configmap: %v", err)
	}
	log.Printf("Created configmap \"%s\".", configmap.GetObjectMeta().GetName())
	defer func() {
		log.Printf("Deleting configmap %s ...", o.Name)
		err := configmapClient.Delete(ctx, o.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("error configmap service: %v\n", err)
		}
	}()

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	// Create the service for incoming traffic within the cluster. The
	// services aceppts traffic on all ports that are in mentioned in
	// o.PortMappings[*].ContainerPortNumber using the
	// specied protocol.
	serviceClient := o.ClientSet.CoreV1().Services(o.Namespace)
	svcPorts := servicePorts(o.PortMappings)
	service := getService(o.Name, svcPorts)
	log.Printf("Creating service \"%s\"...\n", o.Name)
	service, err = serviceClient.Create(ctx, service, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating service: %v", err)
	}
	log.Printf("Created service \"%s\".", service.GetObjectMeta().GetName())
	defer interruptcontext.DoGraceful(ctx, func() {
		log.Printf("Deleting service %s ...", service.Name)
		err := serviceClient.Delete(ctx, service.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("error deleting service: %v\n", err)
		}
	})

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	// Create the service for incoming traffic within the cluster. The pod
	// exposes all ports that are in mentioned in
	// o.PortMappings[*].ContainerPortNumber using the
	// specied protocol. Additionally it exposes port 2222 (or 2223 if
	// 2222 is used already) for ssh connections.
	ports := append(containerPorts(o.PortMappings), corev1.ContainerPort{
		Name:          "ssh",
		ContainerPort: int32(o.RemoteSSHPort),
	})
	podClient := o.ClientSet.CoreV1().Pods(o.Namespace)
	pod := getPod(o.Name, ports)
	log.Printf("Creating pod \"%s\"...\n", o.Name)
	pod, err = podClient.Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating pod: %v", err)
	}
	log.Printf("Created pod \"%s\".", service.GetObjectMeta().GetName())
	defer interruptcontext.DoGraceful(ctx, func() {
		log.Printf("Deleting pod %s ...", pod.Name)
		err := podClient.Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Printf("error deleting pod: %v. That pod probably still runs. You can use dew cleanup to clean up all resources created by dew.", err)
		}
	})

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	// Wait for the pod to be ready before setting up a SSH connection.
	watchOptions := metav1.ListOptions{}
	watchOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", o.Name).String()
	watchOptions.ResourceVersion = pod.GetResourceVersion()
	podWatch, err := podClient.Watch(ctx, watchOptions)
	if err != nil {
		return fmt.Errorf("error watching pod: %v", err)
	}
	// TODO In case of graceful interrupt, wcancel() and return cmd.ErrInterrupted
	// if err == wctx.Err (== context.Cancelled).
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

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	pfwdReadyCh := make(chan struct{}) // closed when portforwarding ready?
	pfwdStopCh := make(chan struct{}, 1)
	pfwdDoneCh := make(chan struct{}) // close when portforwarding exits.
	go func() error {
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
		fw, err := portforward.NewOnAddresses(
			dialer,
			[]string{"localhost"},
			[]string{fmt.Sprintf("%d:%d", o.LocalSSHPort, o.RemoteSSHPort)},
			pfwdStopCh,
			pfwdReadyCh,
			o.Out,    // TODO: var bout bytes.Buffer; buffOut := bufio.NewWriter(&berr)
			o.ErrOut, // TODO: var berr bytes.BUffer; buffErr := bufio.NewWriter(&berr)
		)
		if err != nil {
			return err
		}
		err = fw.ForwardPorts() // blocks
		if err != nil {
			return fmt.Errorf("error Forwarding SSH port: %v\n", err)
		}
		// If this errors, also everything following will error.
		close(pfwdDoneCh)
		return nil
	}()
	defer interruptcontext.DoGraceful(ctx, func() {
		// TODO: if portforward somewhat hangs this will block cleaning up
		// k8s pod and service.
		close(pfwdStopCh)
		<-pfwdDoneCh
		log.Println("port forwarding closed")
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-graceCh:
		return interruptcontext.Interrupted
	case <-pfwdReadyCh:
		log.Printf("port forwarding  is ready: forwarding :%d --> pod %d\n", o.LocalSSHPort, o.RemoteSSHPort)
	}

	// TODO: need to do retries here. It seems the pod does not accept
	// SSH connections also its ready.
	<-time.After(5 * time.Second)

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
		log.Printf("error ssh.Dial:%v\n", err)
		return err
	}
	defer interruptcontext.DoGraceful(ctx, func() {
		log.Printf("ssl connection closed 127.0.0.1:%d\n", o.LocalSSHPort)
		sshClient.Close()
	})
	log.Printf("ssl connection ready 127.0.0.1:%d\n", o.LocalSSHPort)

	select {
	case <-graceCh:
		return interruptcontext.Interrupted
	default:
	}

	// Setup tunnels.
	var tunnels []*tunnel.Tunnel
	for _, m := range o.PortMappings {
		// TODO: we could check for interrupt and ctx.Done in every iteration
		// TODO support remote ips: Note that it does not work without the 0.0.0.0 here.
		remote := fmt.Sprintf("0.0.0.0:%d", m.ContainerPortNumber)
		forward := m.TargetAddress()
		// Create listener for the tunnel. The listener gets closed
		// when the tunnel gets closed.
		lis, err := sshClient.Listen("tcp", remote)
		if err != nil {
			if !o.ContinueOnTunnelError {
				// Close all created tunnels.
				for _, t := range tunnels {
					select {
					case <-ctx.Done():
						// Command killed. Leave tunnels left
						// unclosed.
						return ctx.Err()
					default:
						t.Close()
						<-t.Done()
					}
				}
				return fmt.Errorf("failed to setup listener from %s to %s: %v", remote, forward, err)
			}
			log.Printf("failed to setup listener from %s to %s: %v", remote, forward, err)
		}
		t := tunnel.New(lis, forward)
		tunnels = append(tunnels, t)
		log.Printf("tunnel from remove %s to target %s setup\n", remote, forward)
	}

	// Open tunnels.
	tErrg, tctx := errgroup.WithContext(ctx)
	for _, tt := range tunnels {
		t := tt
		tErrg.Go(t.Open)
	}
	go func() {
		select {
		case <-tctx.Done():
			// If tctx is done and tctx.Err is non-nil an error
			// occured. Close the other tunnels if requested.
			// Note that if ctx is done and and tctx.Err is nil,
			// the Errgroup and thus the tunnels already exited.
			if tctx.Err() != nil && !o.ContinueOnTunnelError {
				for _, t := range tunnels {
					t.Close()
				}
			}
		case <-graceCh:
			for _, t := range tunnels {
				t.Close()
			}
		}
	}()
	_ = tErrg.Wait()

	// Note that, in case of a graceful shutdown the defer functions will
	// close the SSH connection, close the portforwarding and cleanup the
	// pod and services.
	return nil
}

func getConfigMap(name string, remotePort int) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.dew": name,
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

func getPod(name string, ports []corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.dew": name,
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

func getService(name string, ports []corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"io.github.dew": name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"io.github.dew": name,
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
