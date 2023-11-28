* `make exec/curl` works for app-1 and app-2 but we can't route to app-3. Can you fix it?
  * kubectl exec -it deployment/app-2 -c netshoot  -- curl -v http://127.0.0.1:8001/  -H 'service: app-3'
  1. 
     traffic flow:
       ```
                                                                       -----------------------------------------------------
                                                                       |      service app-3                                |
       --------------------------------------------                    | ---------------------------------------------------
       |          app-2                           |                    |  |        app-3                               |  |
       |  | netshoot  |    |       Envoy         ||    9090 ---> 8000  |  |  | envoy               |    | fake-service |  | 
       |  | curl :8001| -> | port 8001 (outbound)||  ----------------> |  |  | port 8000 (inbound) | -> | port 9090 |  |  |
       -------------------------------------------                     |  ------------------------------------------------| 
                                                                       ----------------------------------------------------
       ```
       * Change the targetPort of service app-3 to 8000, same as the Envoy containerPort. As envoy is listening on 8000 (inbound) inside the container, when the traffic comes into the service, we want it to be forwarded to the envoy, and then goes to fake-service
  2.
     traffic flow:
     ```
                                                                     ------------------------|
                                                                     |      service app-3    |
     --------------------------------------------                    |     ---------------
     |          app-2                           |                    |     |   app-3      |  |
     |  | netshoot  |    |       Envoy         ||    9090 ---> 9090  |     | fake-service |  |
     |  | curl :8001| -> | port 8001 (outbound)||  ----------------> |     | port 9090    |  |
     -------------------------------------------                     |     ---------------   |
                                                                      -----------------------
     ```
     * Change the targetPort of service app-3 to 9090, same as the fake-service containerPort. When the traffic comes into the service, forward it to the fake-service directly will also work
* We seem to be updating the config map too often on Kubernetes, can you fix this?
    * Possible reasons we want to fix this:
      * After we edit the config map, we need to do `kubectl rollout restart deployment`, this operation will terminate the old pod, and start a new pod. 
        1. It might cause the application unstable if the pod is restarted too often
        2. It might cause negative influence on the performance of the application if the application need some warm-up after each restart
        3. It might cause temporarily cpu and memory consumption, as the scheduler need to terminate the old one, and start a new one.
    * As there are two kinds of config map, the one is for envoy, another is for the control plane. The question didn't say which one are we going to update frequently, so we consider about this two cases
    * Envoy config map:
      * run a init-container to create a bootstrap.yaml via a shell script, then the envoy container could read the same file in the emptyDir, in this way, the config map is not needed anymore, then we avoid updating the config map
      * To update the configmap less frequently, we could use service annotation to enable this `kubectl annotate service app1`. If we want to apply any changes to the envoy bootstrap config, we add an annotation for that. As there is a control plane keep watching the services, any changes on the services could be apply to the data plane via XDS.
      * Besides annotation, we could also use CRDs to apply network policy, the control plane could watch the changes to CRDs, and apply it to envoy.
    * control plane config map:
      * the controller will only update config map if there is any changes on service, so if we don't changing the services frequently, the config map won't be change frequently
* Add the ability to add an annotation to a service 'mesh-timeout: 2s' which will apply a timeout to requests on the server side (only the service affected should have its configuration modified)
  ```yaml
    apiVersion: v1
    kind: Service
    metadata:
      annotations:
        "mesh-timeout": 2s
      name: app-3
      labels:
      meshed: enabled
    ```
```shell
  root@foo kubectl exec -it deployment/app-3 -c netshoot  -- curl -v http://127.0.0.1:8081/config_dump
  {
          "match": {
           "prefix": "/",
           "headers": [
            {
             "name": "service",
             "string_match": {
              "exact": "app-3"
             }
            }
           ]
          },
          "route": {
           "cluster": "app-3",
           "timeout": "2s"
          },
          "name": "app-3"
  }
```