package main

import (
        "net/http"
        "reflect"
        "time"
        "sort"
        "fmt"
        "context"
        "strings"
        "regexp"
        "os"
        "os/signal"
        "encoding/json"
        "k8s.io/client-go/kubernetes"
        "k8s.io/client-go/rest"
        "k8s.io/apimachinery/pkg/runtime/schema"
        "k8s.io/client-go/dynamic"
        "k8s.io/client-go/dynamic/dynamicinformer"
        "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
        "k8s.io/client-go/tools/cache"
        "github.com/prometheus/client_golang/prometheus"
        "github.com/prometheus/client_golang/prometheus/promauto"
        "github.com/prometheus/client_golang/prometheus/promhttp"
        "github.com/projectcalico/libcalico-go/lib/options"
        "k8s.io/client-go/tools/clientcmd"
        "github.com/projectcalico/libcalico-go/lib/selector/tokenizer"
        "github.com/spf13/viper"
         calicoapi "github.com/projectcalico/libcalico-go/lib/apis/v3"
         calicoclient "github.com/projectcalico/libcalico-go/lib/clientv3"
         neturl "net/url"
         log "github.com/sirupsen/logrus"
         metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

)


type networksetsController struct {
     runtime_viper *viper.Viper
     clientset *kubernetes.Clientset
     calicoClient calicoclient.Interface
     ctx context.Context
     metric_counter map[string]prometheus.Counter
}

type nets_type struct {
      Nets   []string `json:"nets,omitempty" validate:"omitempty,dive,cidr"`
}

func getOneHTTPparamWithEmpty(r *http.Request,param string) string{
        if param, ok := r.Form[param]; ok {
                    return param[0]
        }
        return ""
}



func in_array(a string, list []string) bool {
    for _, b := range list {
        if b == a {
            return true
        }
    }
    return false
}

func AppendIfMissingStr(slice []string, i string) []string {
    for _, ele := range slice {
        if ele == i {
                return slice
        }
    }
    return append(slice, i)
}

func AppendIfMissing(slice []string, i ...string) []string {
    for _, sli := range i {
        slice=AppendIfMissingStr(slice, sli)
    }
    return slice
}




// getClients builds and returns Kubernetes and Calico clients.
func (nc *networksetsController) getClients(kubeconfig string) (*kubernetes.Clientset, calicoclient.Interface, error) {
    // Get Calico client
    calicoClient, err := calicoclient.NewFromEnv()
    if err != nil {
        return nil, nil, fmt.Errorf("failed to build Calico client: %s", err)
    }

    // Now build the Kubernetes client, we support in-cluster config and kubeconfig
    // as means of configuring the client.
    k8sconfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
    if err != nil {
         return nil, nil, fmt.Errorf("failed to build kubernetes client config: %s", err)
    }

    // Get Kubernetes clientset
    k8sClientset, err := kubernetes.NewForConfig(k8sconfig)
    if err != nil {
         return nil, nil, fmt.Errorf("failed to build kubernetes client: %s", err)
    }

    return k8sClientset, calicoClient, nil
}


func (nc *networksetsController) run() {
    config, err := rest.InClusterConfig()
    if err != nil {
        log.Println(err.Error())
    }
    // creates the dynami client
    dc, err := dynamic.NewForConfig(config)
    if err != nil {
        log.Println(err.Error())
    }
    // Create a factory object that we can say "hey, I need to watch this resource"
    // and it will give us back an informer for it
    f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dc, 0, metav1.NamespaceAll, nil)

    //Look by NetworkPolicies
    npr, _ := schema.ParseResourceArg("networkpolicies.v1.crd.projectcalico.org")
    // Finally, create our informer for resource!
    i := f.ForResource(*npr)

    stopChNetworkpolicy := make(chan struct{})
    go nc.startNetworkpolicyWatching(stopChNetworkpolicy, i.Informer())


    //Look by GlobalNetworkPolicies
    gnpr, _ := schema.ParseResourceArg("globalnetworkpolicies.v1.crd.projectcalico.org")
    // Finally, create our informer for resource!
    j := f.ForResource(*gnpr)
    stopChGlobalNetworkpolicy := make(chan struct{})
    go nc.startGlobalNetworkpolicyWatching(stopChGlobalNetworkpolicy, j.Informer())

    sigCh := make(chan os.Signal, 0)
    signal.Notify(sigCh, os.Kill, os.Interrupt)

    <-sigCh
    close(stopChNetworkpolicy)
    close(stopChGlobalNetworkpolicy)
}



func (nc *networksetsController) GetLabelsBySelector(sel string) (map[string][]string,error) {

                 //Get config sections
                 var config_strings []string
                 for label,_ := range nc.runtime_viper.AllSettings() {
                    if (label != "main") {
                       config_strings = append(config_strings, label)
                    }
                 }
                 log.Debug("config strings: ",config_strings)

                 //Get tokens from selector
                 tokens,parse_err := tokenizer.Tokenize(sel)
                 if parse_err!=nil {
                    return map[string][]string{}, parse_err
                 }
                 log.Debug("tokens: ",tokens)

                 //return labels if tokens equal config label
                 out := make(map[string][]string)
                 now_label:=""
                 for _,token := range tokens {
                    token_label:=strings.ToLower(fmt.Sprintf("%v",token.Value))
                    if ( token.Kind == tokenizer.TokLabel ) {
                       if ( in_array(token_label, config_strings ) ) {
                           now_label=fmt.Sprintf("%v",token.Value)
                       } else {
                           now_label = ""
                       }
                    }
                    if ( now_label!="" && token.Kind == tokenizer.TokStringLiteral) {
                       out[now_label] = AppendIfMissing( out[now_label], fmt.Sprintf("%v",token.Value) )
                    }
                 }

                 return out,nil
}

func MergeMaps(base map[string][]string, in map[string][]string ) (map[string][]string) {
              out:=base
              for k,v := range in {
                 out[k] = AppendIfMissing(out[k],v...)
              }
              return out
}


/*
NETWORK POLICIES
*/
func (nc *networksetsController) GetNetworkPolicyRuleLabels( namespace string, np_name string ) ( map[string][]string, error) {

              //Read Network Policy
              out:=make(map[string][]string)
              np, err := nc.calicoClient.NetworkPolicies().Get(nc.ctx, namespace, np_name, options.GetOptions{})
              if err != nil {
                 return out, err
              }
              for _,rule := range np.Spec.Ingress {
                 log.Debug("Ingress selector - ", rule.Source.Selector)

                 lbls,lblerr := nc.GetLabelsBySelector(rule.Source.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)

                 lbls,lblerr = nc.GetLabelsBySelector(rule.Destination.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)

              }
              for _,rule := range np.Spec.Egress {
                 log.Debug("Egress selector - ", rule.Source.Selector)

                 lbls,lblerr := nc.GetLabelsBySelector(rule.Source.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)

                 lbls,lblerr = nc.GetLabelsBySelector(rule.Destination.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)
              }
              log.Debug("labels=",out)
              return out, nil

}

func (nc *networksetsController) SetupNetworksets(namespace string, name string) (error) {

              //Collect all policy in current namespace
              namespace_rules := make(map[string][]string)
              calicoPolicies, err := nc.calicoClient.NetworkPolicies().List(nc.ctx, options.ListOptions{Namespace: namespace})
              if err != nil {
                  return err
              }
              for _, policy := range calicoPolicies.Items {


                      log.Debug("Find policy: ",policy.Name)
                      //Check rules for selector
                      rl,err_rule:=nc.GetNetworkPolicyRuleLabels( namespace, policy.Name )
                      if err_rule != nil {
                             log.Info(err_rule)
                             nc.metric_counter["error_counter"].Inc()
                             continue
                      }
                      //In namespace_rules collect all label vaules
                      namespace_rules = MergeMaps(namespace_rules,rl)
              }
              log.Debug("rule labels = ",namespace_rules)

              //Get NetwrokSets in current namespace
              current_networksets := make(map[string]map[string]bool)
              calicoNetworksets, err := nc.calicoClient.NetworkSets().List(nc.ctx, options.ListOptions{Namespace: namespace})
              if err != nil {
                   return err
              }
              //Find networksets with label from configuration
              for _, networkset := range calicoNetworksets.Items {
                   if (metav1.HasAnnotation(networkset.ObjectMeta,"automatization/networksets-controller")) {
                      meta_labels := networkset.ObjectMeta.GetLabels()
                      for lbl,lbl_slice := range namespace_rules {
                         for _,val := range lbl_slice {
                            if meta_labels[lbl] == val {
                               log.Debug("Find networkset ",networkset.Name," with label ",lbl," = ",val)
                               if _,ok := current_networksets[lbl]; !ok {
                                  current_networksets[lbl] = map[string]bool{val: true}
                               } else {
                                  current_networksets[lbl][val] = true
                               }
                            }
                         }
                      }

                   }
              }
              log.Debug("current networksets ",current_networksets)

              //Create networksets
              for lbl,lbl_slice := range namespace_rules {
                  for _,val := range lbl_slice {
                      if _,ok := current_networksets[lbl][val];!ok {
                          log.Info("Create networkset ",lbl," = ",val)
                          re := regexp.MustCompile(`[^a-z0-9\-]`)
                          name_ns := re.ReplaceAllString(strings.ToLower("auto-ns-"+val),"-")
                          ns, ns_err := nc.NetworksetDef(namespace, name_ns , lbl, val)
                          if ns_err != nil {
                             log.Info(ns_err)
                             nc.metric_counter["error_counter"].Inc()
                             continue
                          }
                          _, err := nc.calicoClient.NetworkSets().Create(nc.ctx, ns, options.SetOptions{})
                          if err != nil {
                             log.Info(err)
                             nc.metric_counter["error_counter"].Inc()
                             continue
                          }
                      }
                  }
              }

              //Delete networksets
              for _, networkset := range calicoNetworksets.Items {
                   if (metav1.HasAnnotation(networkset.ObjectMeta,"automatization/networksets-controller")) {
                      meta_labels := networkset.ObjectMeta.GetLabels()
                      for_delete := true
                      for del_lbl, val_lbl := range meta_labels {
                               if v,ok := current_networksets[del_lbl][val_lbl]; ok && v {
                                  for_delete = false
                                  continue
                               }
                      }
                      if for_delete {
                        ns_name:=networkset.GetName()
                        log.Info("Delete NetworkSet ", ns_name)
                        _, err := nc.calicoClient.NetworkSets().Delete(nc.ctx, namespace, ns_name, options.DeleteOptions{})
                        if err != nil {
                             return err
                        }
                      }
                   }
              }

              return nil
}


func (nc *networksetsController) ReloadNetworksets() (error) {

     var config_strings []string
     for label,_ := range nc.runtime_viper.AllSettings() {
        if (label != "main") {
            config_strings = append(config_strings, label)
        }
     }
     log.Debug("config strings: ",config_strings)

     calicoNetworksets, err := nc.calicoClient.NetworkSets().List(nc.ctx, options.ListOptions{})
     if err != nil {
           return err
     }
     normal:=make(map[string]map[string]*calicoapi.NetworkSet)
     for _, networkset := range calicoNetworksets.Items {
           if (metav1.HasAnnotation(networkset.ObjectMeta,"automatization/networksets-controller")) {
                      meta_labels := networkset.ObjectMeta.GetLabels()
                      for lbl, val := range meta_labels {
                          if in_array(strings.ToLower(lbl),config_strings) {

                             var ns *calicoapi.NetworkSet
                             if _,ok := normal[lbl][val]; ok {
                                 ns = normal[lbl][val]
                             } else {

                                var ns_err error
                                ns, ns_err = nc.NetworksetDef(networkset.ObjectMeta.GetNamespace(), networkset.ObjectMeta.GetName() , lbl, val)
                                if ns_err != nil {
                                   log.Info(ns_err)
                                   nc.metric_counter["error_counter"].Inc()
                                   continue
                                }
                                normal[lbl]=map[string]*calicoapi.NetworkSet{val:ns}
                             }
                             sort.Strings(ns.Spec.Nets)
                             sort.Strings(networkset.Spec.Nets)
                             if !reflect.DeepEqual(ns.Spec.Nets,networkset.Spec.Nets) {
                                 log.Debug("Nets are not in normal state. Update networkset ",networkset.ObjectMeta.GetName())

                                 //Update networkset
                                 ns.ObjectMeta.SetResourceVersion(networkset.ObjectMeta.GetResourceVersion())
                                 ns.ObjectMeta.SetCreationTimestamp(networkset.ObjectMeta.GetCreationTimestamp())
                                 ns.ObjectMeta.SetUID(networkset.ObjectMeta.GetUID())
                                 ns.ObjectMeta.SetNamespace(networkset.ObjectMeta.GetNamespace())
                                 _, err := nc.calicoClient.NetworkSets().Update(nc.ctx, ns, options.SetOptions{})
                                 if err != nil {
                                     log.Info(err)
                                     nc.metric_counter["error_counter"].Inc()
                                     continue
                                 }

                             } else {
                                 log.Debug("Networkset '",networkset.ObjectMeta.GetName(),"' in normal state")
                             }



                          }
                      }
           }
     }
     return nil
}


func (nc *networksetsController) startNetworkpolicyWatching(stopCh <-chan struct{}, s cache.SharedIndexInformer) {


    handlers := cache.ResourceEventHandlerFuncs{
            AddFunc: func(obj interface{}) {

              u := obj.(*unstructured.Unstructured)
              namespace := u.GetNamespace()
              //Hack for strange calico networkpolicy names
              np_name:=strings.Replace(u.GetName(), "default.","",1)

              log.Debug("create event (namespace:",namespace," np:",np_name,")")
              err := nc.SetupNetworksets( namespace, np_name )
              if err!= nil {
                  log.Info(err)
                  nc.metric_counter["error_counter"].Inc()
              }

            },
            UpdateFunc: func(oldObj, obj interface{}) {

              u := obj.(*unstructured.Unstructured)
              namespace := u.GetNamespace()
              //Hack for strange calico networkpolicy names
              np_name:=strings.Replace(u.GetName(), "default.","",1)

              log.Debug("update event (namespace:",namespace," np:",np_name,")")
              err := nc.SetupNetworksets( namespace, np_name )
              if err!= nil {
                  log.Info(err)
                  nc.metric_counter["error_counter"].Inc()
              }


            },
            DeleteFunc: func(obj interface{}) {

              u := obj.(*unstructured.Unstructured)
              namespace := u.GetNamespace()
              //Hack for strange calico networkpolicy names
              np_name:=strings.Replace(u.GetName(), "default.","",1)

              log.Debug("create event (namespace:",namespace," np:",np_name,")")
              err := nc.SetupNetworksets( namespace, np_name )
              if err!= nil {
                  log.Info(err)
                  nc.metric_counter["error_counter"].Inc()
              }

            },
    }

    s.AddEventHandler(handlers)
    s.Run(stopCh)
}

func (nc *networksetsController) NetworksetDef(namespace string, name string, label string, value string) (*calicoapi.NetworkSet,error) {

    url := nc.runtime_viper.GetString(strings.ToLower(label+".url"))
    if url=="" {
      return &calicoapi.NetworkSet{}, fmt.Errorf("Empty url for config [%s]", label)
    }

    //Get nets from source network services
    v := neturl.Values{}
    v.Add( "item", value )
    resp, err := http.PostForm( url, v )
    if err!=nil {
      return &calicoapi.NetworkSet{}, err
    }
    defer resp.Body.Close()
    decoder := json.NewDecoder(resp.Body)
    var nets nets_type
    err_get_nets := decoder.Decode(&nets)
    if err_get_nets != nil {
      return &calicoapi.NetworkSet{}, fmt.Errorf("Fail to parse response from url(%s) %s = %s - %v",url,label,value,err_get_nets)
    }
    log.Debug("Nets from api [",label,"]: ",nets)
    if len(nets.Nets)<1 {
       return &calicoapi.NetworkSet{}, fmt.Errorf("Empty nets for %s = %s",label,value)
    }

    labels := map[string]string{
        label: value,
    }
    return &calicoapi.NetworkSet{
        ObjectMeta: metav1.ObjectMeta{
            Name:      name ,
            Namespace: namespace,
            Labels:    labels,
            Annotations: map[string]string{
                "automatization/networksets-controller": "true",
            },
        },
        Spec: calicoapi.NetworkSetSpec{
           Nets: nets.Nets,
        },
    }, nil
}


/*
GLOBAL NETWORK POLICIES
*/

func (nc *networksetsController) ReloadGlobalNetworksets() (error) {

     var config_strings []string
     for label,_ := range nc.runtime_viper.AllSettings() {
        if (label != "main") {
            config_strings = append(config_strings, label)
        }
     }
     log.Debug("config strings: ",config_strings)

     calicoGlobalNetworksets, err := nc.calicoClient.GlobalNetworkSets().List(nc.ctx, options.ListOptions{})
     if err != nil {
           return err
     }
     normal:=make(map[string]map[string]*calicoapi.GlobalNetworkSet)
     for _, globalnetworkset := range calicoGlobalNetworksets.Items {
           if (metav1.HasAnnotation(globalnetworkset.ObjectMeta,"automatization/networksets-controller")) {
                      meta_labels := globalnetworkset.ObjectMeta.GetLabels()
                      for lbl, val := range meta_labels {
                          if in_array(strings.ToLower(lbl),config_strings) {

                             var gns *calicoapi.GlobalNetworkSet
                             if _,ok := normal[lbl][val]; ok {
                                 gns = normal[lbl][val]
                             } else {

                                var gns_err error
                                gns, gns_err = nc.GlobalNetworksetDef( globalnetworkset.ObjectMeta.GetName() , lbl, val)
                                if gns_err != nil {
                                   log.Info(gns_err)
                                   nc.metric_counter["error_counter"].Inc()
                                   continue
                                }
                                normal[lbl]=map[string]*calicoapi.GlobalNetworkSet{val:gns}
                             }

                             sort.Strings(gns.Spec.Nets)
                             sort.Strings(globalnetworkset.Spec.Nets)
                             if !reflect.DeepEqual(gns.Spec.Nets,globalnetworkset.Spec.Nets) {
                                 log.Debug("Nets are not in normal state. Update global networkset ",globalnetworkset.ObjectMeta.GetName())

                                 //Update networkset
                                 gns.ObjectMeta.SetResourceVersion(globalnetworkset.ObjectMeta.GetResourceVersion())
                                 gns.ObjectMeta.SetCreationTimestamp(globalnetworkset.ObjectMeta.GetCreationTimestamp())
                                 gns.ObjectMeta.SetUID(globalnetworkset.ObjectMeta.GetUID())
                                 _, err := nc.calicoClient.GlobalNetworkSets().Update(nc.ctx, gns, options.SetOptions{})
                                 if err != nil {
                                     log.Info(err)
                                     nc.metric_counter["error_counter"].Inc()
                                     continue
                                 }

                             } else {
                                 log.Debug("GlobalNetworkset '",globalnetworkset.ObjectMeta.GetName(),"' in normal state")
                             }



                          }
                      }
           }
     }
     return nil
}


func (nc *networksetsController) GlobalNetworksetDef( name string, label string, value string) (*calicoapi.GlobalNetworkSet,error) {

    url := nc.runtime_viper.GetString(strings.ToLower(label+".url"))
    if url=="" {
      return &calicoapi.GlobalNetworkSet{}, fmt.Errorf("Empty url for config [%s]", label)
    }

    //Get nets from source network services
    v := neturl.Values{}
    v.Add( "item", value )
    resp, err := http.PostForm( url, v )
    if err!=nil {
      return &calicoapi.GlobalNetworkSet{}, err
    }
    defer resp.Body.Close()
    decoder := json.NewDecoder(resp.Body)
    var nets nets_type
    err_get_nets := decoder.Decode(&nets)
    if err_get_nets != nil {
      return &calicoapi.GlobalNetworkSet{}, fmt.Errorf("Fail to parse response from url(%s) %s = %s - %v",url,label,value,err_get_nets)
    }
    log.Debug("Nets from api [",label,"]: ",nets)
    if len(nets.Nets)<1 {
       return &calicoapi.GlobalNetworkSet{}, fmt.Errorf("Empty nets for %s = %s",label,value)
    }

    labels := map[string]string{
         label: value,
    }
    return &calicoapi.GlobalNetworkSet{
        ObjectMeta: metav1.ObjectMeta{
            Name:      name ,
            Labels:    labels,
            Annotations: map[string]string{
               "automatization/networksets-controller": "true",
            },
        },
        Spec: calicoapi.GlobalNetworkSetSpec{
           Nets: nets.Nets,
        },
    }, nil
}


func (nc *networksetsController) GetGlobalNetworkPolicyRuleLabels( np_name string ) ( map[string][]string, error) {

              //Read Network Policy
              out:=make(map[string][]string)
              np, err := nc.calicoClient.GlobalNetworkPolicies().Get(nc.ctx, np_name, options.GetOptions{})
              if err != nil {
                 return out, err
              }
              for _,rule := range np.Spec.Ingress {
                 log.Debug("Ingress selector - ", rule.Source.Selector)

                 lbls,lblerr := nc.GetLabelsBySelector(rule.Source.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)

                 lbls,lblerr = nc.GetLabelsBySelector(rule.Destination.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)

              }
              for _,rule := range np.Spec.Egress {
                 log.Debug("Egress selector - ", rule.Source.Selector)

                 lbls,lblerr := nc.GetLabelsBySelector(rule.Source.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)

                 lbls,lblerr = nc.GetLabelsBySelector(rule.Destination.Selector)
                 if lblerr != nil {
                    return out, lblerr
                 }
                 out=MergeMaps(out,lbls)
              }
              log.Debug("global labels=",out)
              return out, nil

}



func (nc *networksetsController) SetupGlobalNetworksets(name string) (error) {

              //Collect all policy in current namespace
              global_rules := make(map[string][]string)
              calicoPolicies, err := nc.calicoClient.GlobalNetworkPolicies().List(nc.ctx, options.ListOptions{})
              if err != nil {
                  return err
              }
              for _, policy := range calicoPolicies.Items {

                      log.Debug("Find global policy: ",policy.Name)
                      //Check rules for selector
                      rl,err_rule:=nc.GetGlobalNetworkPolicyRuleLabels( policy.Name )
                      if err_rule != nil {
                             log.Info(err_rule)
                             nc.metric_counter["error_counter"].Inc()
                             continue
                      }
                      //In namespace_rules collect all label vaules
                      global_rules = MergeMaps(global_rules,rl)
              }
              log.Debug("global rule labels = ",global_rules)

              //Get GlobalNetwrokSets 
              current_globalnetworksets := make(map[string]map[string]bool)
              calicoGlobalNetworksets, err := nc.calicoClient.GlobalNetworkSets().List(nc.ctx, options.ListOptions{})
              if err != nil {
                   return err
              }
              //Find networksets with label from configuration
              for _, globalnetworkset := range calicoGlobalNetworksets.Items {
                   if (metav1.HasAnnotation(globalnetworkset.ObjectMeta,"automatization/networksets-controller")) {
                      meta_labels := globalnetworkset.ObjectMeta.GetLabels()
                      for lbl,lbl_slice := range global_rules {
                         for _,val := range lbl_slice {
                            if meta_labels[lbl] == val {
                               log.Debug("Find global networkset ",globalnetworkset.Name," with label ",lbl," = ",val)
                               if _,ok := current_globalnetworksets[lbl]; !ok {
                                  current_globalnetworksets[lbl] = map[string]bool{val: true}
                               } else {
                                  current_globalnetworksets[lbl][val] = true
                               }
                            }
                         }
                      }

                   }
              }
              log.Debug("current global networksets ",current_globalnetworksets)

              //Create networksets
              for lbl,lbl_slice := range global_rules {
                  for _,val := range lbl_slice {
                      if _,ok := current_globalnetworksets[lbl][val];!ok {
                          log.Info("Create global networkset ",lbl," = ",val)
                          re := regexp.MustCompile(`[^a-z0-9\-]`)
                          name_ns := re.ReplaceAllString(strings.ToLower("auto-ns-"+val),"-")
                          ns, ns_err := nc.GlobalNetworksetDef( name_ns , lbl, val)
                          if ns_err != nil {
                             log.Info(ns_err)
                             nc.metric_counter["error_counter"].Inc()
                             continue
                          }
                          _, err := nc.calicoClient.GlobalNetworkSets().Create(nc.ctx, ns, options.SetOptions{})
                          if err != nil {
                             log.Info(err)
                             nc.metric_counter["error_counter"].Inc()
                             continue
                          }
                      }
                  }
              }

              //Delete networksets
              for _, globalnetworkset := range calicoGlobalNetworksets.Items {
                   if (metav1.HasAnnotation(globalnetworkset.ObjectMeta,"automatization/networksets-controller")) {
                      meta_labels := globalnetworkset.ObjectMeta.GetLabels()
                      for_delete := true
                      for del_lbl, val_lbl := range meta_labels {
                               if v,ok := current_globalnetworksets[del_lbl][val_lbl]; ok && v {
                                  for_delete = false
                                  continue
                               }
                      }
                      if for_delete {
                        ns_name:=globalnetworkset.GetName()
                        log.Info("Delete GlobalNetworkSet ", ns_name)
                        _, err := nc.calicoClient.GlobalNetworkSets().Delete(nc.ctx,  ns_name, options.DeleteOptions{})
                        if err != nil {
                             return err
                        }
                      }
                   }
              }

              return nil
}


func (nc *networksetsController) startGlobalNetworkpolicyWatching(stopCh <-chan struct{}, s cache.SharedIndexInformer) {


    handlers := cache.ResourceEventHandlerFuncs{
            AddFunc: func(obj interface{}) {

              u := obj.(*unstructured.Unstructured)
              //Hack for strange calico networkpolicy names
              gnp_name:=strings.Replace(u.GetName(), "default.","",1)

              log.Debug("create event global network policy "," gnp:",gnp_name,")")
              err := nc.SetupGlobalNetworksets(  gnp_name )
              if err!= nil {
                  log.Info(err)
                  nc.metric_counter["error_counter"].Inc()
              }

            },
            UpdateFunc: func(oldObj, obj interface{}) {

              u := obj.(*unstructured.Unstructured)
              //Hack for strange calico networkpolicy names
              gnp_name:=strings.Replace(u.GetName(), "default.","",1)

              log.Debug("create event global network policy "," gnp:",gnp_name,")")
              err := nc.SetupGlobalNetworksets(  gnp_name )
              if err!= nil {
                  log.Info(err)
                  nc.metric_counter["error_counter"].Inc()
              }


            },
            DeleteFunc: func(obj interface{}) {

              u := obj.(*unstructured.Unstructured)
              //Hack for strange calico networkpolicy names
              gnp_name:=strings.Replace(u.GetName(), "default.","",1)

              log.Debug("create event global network policy "," gnp:",gnp_name,")")
              err := nc.SetupGlobalNetworksets(  gnp_name )
              if err!= nil {
                  log.Info(err)
                  nc.metric_counter["error_counter"].Inc()
              }

            },
    }

    s.AddEventHandler(handlers)
    s.Run(stopCh)
}




func (nc *networksetsController) Init() (error){

        nc.runtime_viper = viper.New()
        nc.runtime_viper.SetConfigName("networksets-controller")
        nc.runtime_viper.AddConfigPath("/config-k8s")
        nc.runtime_viper.WatchConfig()
        err := nc.runtime_viper.ReadInConfig()
        if err != nil {
           fmt.Printf("Fatal error config file: %s \n", err)
           return err
        }

        //Get clients for calico and k8s
        var err_calico error
        nc.clientset, nc.calicoClient, err_calico = nc.getClients("")
        if err_calico != nil {
           return err_calico
        }

        nc.ctx = context.Background()

        is_debug := nc.runtime_viper.GetBool("main.debug")
        if is_debug {
          log.SetLevel(log.DebugLevel)
        }

        //Prometheus metrics
        nc.metric_counter = make(map[string]prometheus.Counter)

        nc.metric_counter["error_counter"] = promauto.NewCounter(prometheus.CounterOpts{
           Name: "networksets_controller_errors",
           Help: "Count of errors",
        })


        go nc.run()

        reload_time := nc.runtime_viper.GetDuration("main.reload_time")
        ticker := time.NewTicker(time.Millisecond * reload_time)
        go func() {
          for _ = range ticker.C {
            err_reload := nc.ReloadNetworksets()
            if err_reload != nil {
               log.Info(err_reload)
               nc.metric_counter["error_counter"].Inc()
            }
          }
        }()

        globalticker := time.NewTicker(time.Millisecond * reload_time)
        go func() {
          for _ = range globalticker.C {
            err_reload := nc.ReloadGlobalNetworksets()
            if err_reload != nil {
               log.Info(err_reload)
               nc.metric_counter["error_counter"].Inc()
            }
          }
        }()

        return nil
}

func main() {


        var nc networksetsController
        err:=nc.Init()
        if err!=nil {
           log.Info("Fail to start networksets controller", err)
           return
        }

        http.Handle("/metrics", promhttp.Handler())

        service_port := nc.runtime_viper.GetString("main.service_port")

        log.Fatal(http.ListenAndServe(":"+service_port, nil))
}
