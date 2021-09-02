# kubetnl

kubetnl (*kube tunnel*) is a command line utility to tunnel TCP connections from within a Kubernetes to a cluster-external endpoint, e.g. to your local machine.
You can think of it as doing the opposite of `kubectl port-forward`.

## Demo

![kubetnl-demo.gif](https://gist.githubusercontent.com/fischor/6d175f01db8ded817d5fc72dcd37811e/raw/d5a708324354b49fa5dd15c47f9fd52287c394e1/kubetnl.gif)

## `kubetnl --help`

```sh
$ kubetnl --help
kubetnl tunnels TCP connections from within a Kubernetes cluster to an external endpoint.

 Find more information and check out the souce code at: https://github.com/fischor/kubetnl

Basic commands
  tunnel      Setup a new tunnel
  cleanup     Delete all resources created by kubetnl

Other Commands:
  completion  generate the autocompletion script for the specified shell
  version     Print the kubetnl version

Usage:
  kubetnl [flags] [options]

Use "kubetnl <command> --help" for more information about a given command.
Use "kubetnl options" for a list of global command-line options (applies to all commands).
```

## `kubetnl tunnel --help`

```sh
$ kubetnl tunnel --help
Setup a new tunnel.

 A tunnel forwards connections directed to a Kubernetes Service port within a cluster to an endpoint outside of the
cluster, e.g. to your local machine.

 Under the hood "kubetnl tunnel" creates a new service and pod that expose the specified ports. Any incoming connections
to an exposed port of the newly created service/pod will be tunneled to the endpoint specified for that port.

 "kubetnl tunnel" runs in the foreground. To stop press CTRL+C once. This will gracefully shutdown all active
connections and cleanup the created resources in the cluster before exiting.

Examples:
  # Tunnel to local port 8080 from myservice.<namespace>.svc.cluster.local:80.
  kubetnl tunnel myservice 8080:80

  # Tunnel to 10.10.10.10:3333 from myservice.<namespace>.svc.cluster.local:80.
  kubetnl tunnel myservice 10.10.10.10:3333:80

  # Tunnel to local port 8080 from myservice.<namespace>.svc.cluster.local:80 and to local port 9090 from myservice.<namespace>.svc.cluster.local:90.
  kubetnl tunnel myservice 8080:80 9090:90

  # Tunnel to local port 80 from myservice.<namespace>.svc.cluster.local:80 using version 0.1.0 of the kubetnl server
image.
  kubetnl tunnel --image docker.io/fischor/kubetnl-server:0.1.0 myservice 80:80

Options:
      --image='docker.io/fischor/kubetnl-server:0.1.0': The container image thats get deployed to serve a SSH server

Usage:
  kubetnl tunnel SERVICE_NAME TARGET_ADDR:SERVICE_PORT [...[TARGET_ADDR:SERVICE_PORT]] [flags] [options]

Use "kubetnl options" for a list of global command-line options (applies to all commands).
```

# Alternatives

Since you are probably here in search for tools that allow you to forward traffic from within your cluster to the outside: here is a list of alternative tools that achieve similiar things as kubetnl and that might are what you are looking for:

## Telepresence

[Telepresence](https://github.com/telepresenceio/telepresence) will forward traffic from within your cluster to your local machine and also forward traffic from your local machine to the cluster. Also any environment variables and volume mounts (not sure about that though) will be available on your local machine once "connected". If you are looking to setup a local development environment for microservice development, Telepresense is probably the right choice. 

You still might choose `kubetnl` because:

With Telepresence, you need a to setup a Deployment manually before anything can be forwarded. Telepresence will then inject sidecar containers into the pods of that Deployment that are responsible for forwarding connections. `kubetnl` on the other hand creates a new service and pod for you, so no Deployment needs to be set up before.

Telepresence will have to create a new namespace with a *Traffic manager* deployment (but setting it up and tearing it down is super easy) before anything can be forwarded. With `kubectl` there is no extra setup needed.

Telepresence forwards traffic from within the cluster only to your local machine, not to external endpoints (like `kubetnl` does).

## VSCode Bridge to Kubernetes

[VSCode Bridge to Kubernetes](https://docs.microsoft.com/en-us/visualstudio/bridge/bridge-to-kubernetes-vs-code) is quiet a similiar tool to Telepresense. 
It also allows you to forward traffic from your cluster to your local machine as well as the other way around making it a good environment for microservice development. 
For Brige to Kubernetes you need to have an existing pod (and service) set up. 
That pod gets replaced with a pod thats responsible for forwarding the traffic back and forth between the cluster and your local machine. 

However, this only works as a VS Code extension and external endpoints are not supported.

