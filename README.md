# kubetnl

kubetnl (*kube tunnel*) is a command line utility to tunnel TCP connections from within a Kubernetes to a cluster-external endpoint, e.g. to your local machine.
You can think of it as doing the opposite of `kubectl port-forward`.

![kubetnl-demo.gif](https://gist.githubusercontent.com/fischor/6d175f01db8ded817d5fc72dcd37811e/raw/d5a708324354b49fa5dd15c47f9fd52287c394e1/kubetnl.gif)

# `kubetnl --help`

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

# `kubetnl tunnel --help`

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

Since you are probably here in search for looking into tools that allow you to forward traffic from your cluster, here a list of tools that achieve similiar things as kubetnl:

## Telepresence

With Telepresence, 
- you need a Deployment setup before
- you will have to create a new namespace with a Traffic agent deployment (but setting it up and tearing it down is super easy)
- supports forward traffic from within the cluster only to local ports
- when running `telepresence connect` besides forwarding traffic from a service to your local machine, also all services in the cluster are reachable by the k8s dns names: `<svc>.<ns>.cluster.svc.`
- its a really good tool for development

## VSCode Bridge to Kubernetes

Only works with VS Code though.

https://docs.microsoft.com/en-us/visualstudio/bridge/bridge-to-kubernetes-vs-code
