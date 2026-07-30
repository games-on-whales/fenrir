## Development  

### Pre-requisites
Before starting development you need to install:  
* make: often comes pre-installed in most linux distros  
* docker: Install according to your distro  
* golang: Install according to your distro  
* kind: https://kind.sigs.k8s.io/docs/user/quick-start/#installation  
* skaffold: https://skaffold.dev/docs/install/  

This is a simple guide for setting up a development environment for fenrir:  
* First start by creating the cluster:  
    * `make cluster-create`  
* Next setup the dependencies such as cert-manager and metallb alongside the ip pool:  
    * `make cluster-setup`  
* Afterwards you need to apply one of the two gpu generic devices:  
    * `kubectl apply -f hack/additions/amd-generic-device-plugin.yaml`  
    * `kubectl apply -f hack/additions/nvidia-generic-device-plugin.yaml`  
* Finally install the additional generic devices which include uinput and any future dependencies:  
    * `kubectl apply -f hack/additions/generic-device-plugin.yaml`  

This completes the initialization of the kind cluster, you are now able to develop individual components in fenrir.  

Depending on which component you wish to develop:
* Moonlight-Proxy and Direwolf-Operator:  
    * `make dev-operators`  
* Wolf-Agent:  
    * `make dev-agent`  

afterwards, get the moonlight proxy LoadBalancer IP Address: `kubectl get services -n direwolf-dev`  
Pairing with the moonlight-proxy throws the pairing URL in the logs, which you can use to authenticate  
You will then need to add your pairing ID to the profile manifests (this is not final, it'll be subject to change later)  
to get the Pairing name: `kubectl get pairings.direwolf.games-on-whales.github.io --show-labels -n direwolf-dev`
You can change / update the manifest at `hack/resources/profile.yaml` to include your pairing ID so that whenever you pair you get quick access to the apps  

Additional Notes:  
* Any changes to the CRD structs will require you to regenrate the code through: `make codegen`