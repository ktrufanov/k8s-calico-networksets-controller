# k8s-calico-networksets-controller

With this controller, you can control access to custom resources by their names<br>
The addresses of these resources can be obtained from your own dedicated service<br>
For example, it can be salt hosts, net prefixes, dns names...<br>
Here is an example with dns names<br>
Service for dns resolving - [dns-resolver](https://github.com/ktrufanov/dns-resolver)

## Install
quick start
```
kubectl create ns calico-networksets-controller
kubectl apply -f  https://raw.githubusercontent.com/ktrufanov/dns-resolver/0.0.1/k8s-mainfest.yaml
kubectl apply -f  https://raw.githubusercontent.com/ktrufanov/k8s-calico-networksets-controller/0.0.11-1/k8s-mainfest.yaml
```

## Description
Controller watch by create/update/delete [Calico NetworkPolicy](https://docs.projectcalico.org/reference/resources/networkpolicy).<br>
If source/destination selector of NetworkPolicy have the specific label then controller creates/updates [Calico NetworkSet](https://docs.projectcalico.org/reference/resources/networkset).<br>
IP networks/CIDRs for NetworkSet are requested from the http url. This url is customizable for specific label.<br>
Controller periodically updates the NetworkSet.<br>
Аlso works with GlobalNetworkPolicy/GlobalNetworkSet.

***/config-k8s/networksets-controller.toml:***
```
[main]
reload_time=60000
debug=false
service_port=8080

[DNS_RESOLVER]
url="http://dns-resolver.calico-networksets-controller:8080/dns"

[OTHER_NET_SOURCE]
url="http://other-net-source.calico-networksets-controller:8080/nets"
```
***main*** section:
- ***reload-time*** - time to reload networksets in ms
- ***debug*** - debug log
- ***service_port*** - service listen port

The names of other sections are specific labels<br>
- ***DNS_RESOLVER***, ***OTHER_NET_SOURCE*** - specific labels
- ***url*** - net source for specific label

http request example:
```
curl -X POST --data "item=github.com"  http://dns-resolver.calico-networksets-controller:8080/dns
```
response must be in format:
```
{"Nets":["140.82.121.4/32"]}
```
## Example
NetworkPolicy:
```
apiVersion: crd.projectcalico.org/v1
kind: NetworkPolicy
metadata:
  name: default.test-allow-github
  namespace: examples
spec:
  selector: app == 'k8s-example'
  types:
  - Egress
  egress:
  - action: Allow
    protocol: TCP
    destination:
      selector: DNS_RESOLVER == 'github.com'
      ports:
      - 443
```
This NetworkPolicy allow tcp port 443 to github.com

NetworkSet will be create for this NetworkPolicy :
```
apiVersion: crd.projectcalico.org/v1
kind: NetworkSet
metadata:
  annotations:
    automatization/networksets-controller: "true"
  labels:
      DNS_RESOLVER: github.com
  name: auto-ns-github-com
  namespace: examples
spec:
  nets:
  - 140.82.121.3/32
```
This NetworkSet will be refreshed every reload_time times

## Metrics
- ***networksets_controller_errors*** - "Count of errors"
