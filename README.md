# scarab

The agent side of a self-hosted Pi coding platform. Scarab runs the workspace agent and gives it a controlled way to start and manage subagents.

## How it fits together

- **town** is the signed-in management interface.
- **pestilence** authenticates users and creates and removes workspaces.
- **scarab** runs inside a workspace and brokers requests from the agent to Kubernetes.

Workspaces are isolated from one another. Agents request actions such as starting a subagent; they do not receive Kubernetes credentials or direct access to Kubernetes.

## What’s here

- The Pi agent and workspace container images
- The broker that starts and manages subagents
- The `platform-tools` extension used by the root agent

## Development

```sh
bash scripts/verify.sh
```

## Deploying

The Helm chart lives in [helm-charts](https://github.com/gobackto-work/helm-charts). This repository builds and publishes an image; that repository names the version it deploys.

```sh
helm install platform oci://ghcr.io/gobackto-work/charts/platform --version 1.0.0
```


See [Architecture](docs/architecture.md) for the interface between scarab and pestilence, and [Verification](docs/verification.md) for the checks run by the development gate.
