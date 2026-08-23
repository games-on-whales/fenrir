## Wolf-Agent DRA
The wolf agent DRA aims to use GOW's wolf container to create wayland sockets and pulse audio sinks for use in Pods.  

It achieves this by creating a sidecar `wolf-dra` that manages lobby creation and tracks the wolf created sockets.  

before running any of these test pods, you need to configure and apply a device class and reference it.  
I would advice applying the `hack/resources/device_classes/default_device_class.yaml` file if you're on a single GPU node.  
Otherwise go check your `/dev/dri` directory and pick a render node, then set it in `hack/resources/device_classes/multi_gpu_class.yaml` before applying it.  