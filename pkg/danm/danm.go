package main

import (
  "errors"
  "fmt"
  "log"
  "net"
  "os"
  "strconv"
  "strings"
  "encoding/json"
  "reflect"
  "github.com/satori/go.uuid"
  "github.com/containernetworking/cni/pkg/skel"
  "github.com/containernetworking/cni/pkg/types"
  "github.com/containernetworking/cni/pkg/version"
  "github.com/containernetworking/cni/pkg/types/current"
  meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
  k8s "k8s.io/apimachinery/pkg/types"
  "k8s.io/client-go/rest"
  "k8s.io/client-go/tools/clientcmd"
  "k8s.io/client-go/kubernetes"
  danmtypes "github.com/nokia/danm/pkg/crd/apis/danm/v1"
  danmclientset "github.com/nokia/danm/pkg/crd/client/clientset/versioned"
  "github.com/nokia/danm/pkg/danmep"
  "github.com/nokia/danm/pkg/ipam"
  "github.com/nokia/danm/pkg/cnidel"
  "github.com/nokia/danm/pkg/syncher"
  checkpoint_utils "github.com/intel/multus-cni/checkpoint"
  checkpoint_types "github.com/intel/multus-cni/types"
  sriov_utils "github.com/intel/sriov-cni/pkg/utils"
)

var (
  apiHost = os.Getenv("API_SERVERS")
  danmApiPath = "danm.k8s.io"
  danmIfDefinitionSyntax = danmApiPath + "/interfaces"
  v1Endpoint = "/api/v1/"
  cniVersion = "0.3.1"
  kubeConf string
  defaultNetworkName = "default"
)

type NetConf struct {
  types.NetConf
  Kubeconfig string `json:"kubeconfig"`
}

// K8sArgs is the valid CNI_ARGS type used to parse K8s CNI event calls (thanks Multus)
type K8sArgs struct {
  types.CommonArgs
  IP                         net.IP
  K8S_POD_NAME               types.UnmarshallableString
  K8S_POD_NAMESPACE          types.UnmarshallableString
  K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

type cniArgs struct {
  nameSpace string
  netns string
  podId string
  containerId string
  annotation map[string]string
  labels map[string]string
  stdIn []byte
  interfaces []danmtypes.Interface
  podUid k8s.UID
}

func createInterfaces(args *skel.CmdArgs) error {
  cniArgs,err := extractCniArgs(args)
  if err != nil {
    log.Println("ERROR: ADD: CNI args cannot be loaded with error:" + err.Error())
    return fmt.Errorf("CNI args cannot be loaded with error: %v", err)
  }
  log.Println("CNI ADD invoked with: ns:" + cniArgs.nameSpace + " PID:" + cniArgs.podId + " CID: " + cniArgs.containerId)
  if err = getPodAttributes(cniArgs); err != nil {
    log.Println("ERROR: ADD: Pod manifest could not be parsed with error:" + err.Error())
    return fmt.Errorf("Pod manifest could not be parsed with error: %v", err)
  }
  extractConnections(cniArgs)
  if len(cniArgs.interfaces) == 1 && cniArgs.interfaces[0].Network == defaultNetworkName {
    log.Println("WARN: ADD: no network connections for Pod: " + cniArgs.podId + " are defined in spec.metadata.annotation. Falling back to use: " + defaultNetworkName)
  }
  cniResult, err := setupNetworking(cniArgs)
  if err != nil {
    //Best effort cleanup - not interested in possible errors, anyway could not do anything with them
    os.Setenv("CNI_COMMAND","DEL")
    deleteInterfaces(args)
    log.Println("ERROR: ADD: CNI network could not be set up with error:" + err.Error())
    return fmt.Errorf("CNI network could not be set up: %v", err)
  }
  return types.PrintResult(cniResult, cniVersion)
}

func createDanmClient(stdIn []byte) (danmclientset.Interface,error) {
  config, err := getClientConfig(stdIn)
  if err != nil {
    return nil, errors.New("Parsing kubeconfig failed with error:" + err.Error())
  }
  client, err := danmclientset.NewForConfig(config)
  if err != nil {
    return nil, errors.New("Creation of K8s Danm REST client failed with error:" + err.Error())
  }
  return client, nil
}

func getClientConfig(stdIn []byte) (*rest.Config, error){
  confArgs, err := loadNetConf(stdIn)
  if err != nil {
    return nil, err
  }
  kubeConf = confArgs.Kubeconfig
  config, err := clientcmd.BuildConfigFromFlags("", kubeConf)
  if err != nil {
    return nil, err
  }
  return config, nil
}

func loadNetConf(bytes []byte) (*NetConf, error) {
  netconf := &NetConf{}
  err := json.Unmarshal(bytes, netconf)
  if err != nil {
    return nil, errors.New("failed to load netconf:" + err.Error())
  }
  return netconf, nil
}

func extractCniArgs(args *skel.CmdArgs) (*cniArgs,error) {
  kubeArgs := K8sArgs{}
  err := types.LoadArgs(args.Args, &kubeArgs)
  if err != nil {
    return nil,err
  }
  cmdArgs := cniArgs{string(kubeArgs.K8S_POD_NAMESPACE),
                     args.Netns,
                     string(kubeArgs.K8S_POD_NAME),
                     string(kubeArgs.K8S_POD_INFRA_CONTAINER_ID),
                     nil,
                     nil,
                     args.StdinData,
                     nil,
                     "",
                    }
  return &cmdArgs, nil
}

func getPodAttributes(args *cniArgs) error {
  confArgs, err := loadNetConf(args.stdIn)
  if err != nil {
    return errors.New("cannot load CNI NetConf due to error:" + err.Error())
  }
  k8sClient, err := createK8sClient(confArgs.Kubeconfig)
  if err != nil {
    return errors.New("cannot create K8s REST client due to error:" + err.Error())
  }
  pod, err := k8sClient.CoreV1().Pods(string(args.nameSpace)).Get(string(args.podId), meta_v1.GetOptions{})
  if err != nil {
    return errors.New("failed to get Pod info from K8s API server due to:" + err.Error())
  }
  args.annotation = pod.Annotations
  args.labels = pod.Labels
  args.podUid = pod.UID
  return nil
}

func createK8sClient(kubeconfig string) (kubernetes.Interface, error) {
  config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
  if err != nil {
    return nil, err
 }
 return kubernetes.NewForConfig(config)
}

func extractConnections(args *cniArgs) error {
  var ifaces []danmtypes.Interface
  for key, val := range args.annotation {
    if strings.Contains(key, danmIfDefinitionSyntax) {
      err := json.Unmarshal([]byte(val), &ifaces)
      if err != nil {
        return errors.New("Can't create network interfaces for Pod: " + args.podId + " due to badly formatted " + danmIfDefinitionSyntax + " definition in Pod annotation")
      }
      break
    }
  }
  if len(ifaces) == 0 {
    ifaces = []danmtypes.Interface{{Network: defaultNetworkName}}
  }
  args.interfaces = ifaces
  return nil
}

func getResourcePrefix(args *cniArgs, resourceType string)(string,error){
  confArgs, err := loadNetConf(args.stdIn)
  if err != nil {
    return "", errors.New("cannot load CNI NetConf due to error:" + err.Error())
  }
  k8sClient, err := createK8sClient(confArgs.Kubeconfig)
  if err != nil {
    return "", errors.New("cannot create K8s REST client due to error:" + err.Error())
  }
  configmap, err := k8sClient.CoreV1().ConfigMaps(string("kube-system")).Get(string("resource-prefix-map"), meta_v1.GetOptions{})
  if err != nil {
    return "", errors.New("failed to get 'resource-prefix-map' Config Map data from K8s API server due to:" + err.Error())
  }
  return  configmap.Data[resourceType], nil
}

func getRegisteredDevices(args *cniArgs, cniType string)([]string,error){
  resourceMap := make(map[string]*checkpoint_types.ResourceInfo)
  if string(args.podUid) != "" {
    checkpoint, err := checkpoint_utils.GetCheckpoint()
    if err != nil {
      return nil, errors.New("failed to instantiate checkpoint object due to:" + err.Error())
    }
    resourceMap, err = checkpoint.GetComputeDeviceMap(string(args.podUid))
    if err != nil {
      return nil, errors.New("failed to retrieve Pod info from checkpoint object due to:" + err.Error())
    }
  }
  prefix, err := getResourcePrefix(args, cniType)
  if err != nil {
    return nil, errors.New("failed to get resource prefix from config map due to:" + err.Error())
  }
  var registeredDevices []string
  for resourcename, resources := range resourceMap {
    if strings.HasPrefix(resourcename,prefix) {
      registeredDevices = append(registeredDevices, resources.DeviceIDs...)
    }
  }
  return registeredDevices, nil
}

func getSriovInterfaces(args *cniArgs)(map[string]int,map[string]string,error){
  danmClient, err := createDanmClient(args.stdIn)
  if err != nil {
    return nil, nil, errors.New("failed to create DanmClient due to:" + err.Error())
  }
  sriovInterfaces := make(map[string]int)
  interfaceDeviceMap := make(map[string]string)
  for _, interfac := range args.interfaces {
    danmnet, err := danmClient.DanmV1().DanmNets(args.nameSpace).Get(interfac.Network, meta_v1.GetOptions{})
    if err != nil || danmnet.ObjectMeta.Name == ""{
      return nil, nil, errors.New("NID:" + interfac.Network + " in namespace:" + args.nameSpace + " cannot be GET from K8s API server!")
    }
    if danmnet.Spec.NetworkType == "sriov" {
      sriovInterfaces[interfac.Network]++
      interfaceDeviceMap[interfac.Network] = danmnet.Spec.Options.Device
    }
  }
  return sriovInterfaces, interfaceDeviceMap, nil
}

func validateSriovNetworkRequests(sriovInterfaces map[string]int, sriovDevices []string, interfaceDeviceMap map[string]string) error {
  requiredVfonPf := make(map[string]int)
  requestedVfonPf := make(map[string]int)
  for interfac, count := range sriovInterfaces {
    requiredVfonPf[interfaceDeviceMap[interfac]] = count
  }
  for _, device := range sriovDevices {
    pf, err := sriov_utils.GetPfName(device)
    if err != nil {
      return errors.New("failed to get the name of the sriov PF for device "+ device +" due to:" + err.Error())
    }
    requestedVfonPf[pf]++
  }
  eq := reflect.DeepEqual(requiredVfonPf, requestedVfonPf)
  if eq {
    return nil
  }
  log.Printf("Required SR IOV resources: %v", requiredVfonPf)
  log.Printf("Requested SR IOV resources: %v", requestedVfonPf)
  return errors.New("requested and required sriov resources are not matching in the Pod definition")
}

func setupNetworking(args *cniArgs) (*current.Result, error) {
  sriovInterfaces, interfaceDeviceMap, err := getSriovInterfaces(args)
  if err != nil {
    return nil, errors.New("failed to collect sriov interfaces due to:" + err.Error())
  }
  if len(sriovInterfaces) > 0 {
    sriovDevices, err := getRegisteredDevices(args, "sriov")
    if err != nil {
      return nil, errors.New("failed to collect sriov interfaces due to:" + err.Error())
    }
    err = validateSriovNetworkRequests(sriovInterfaces, sriovDevices, interfaceDeviceMap)
    if err != nil {
      return nil, errors.New("sriov resource validation failed due to:" + err.Error())
    }
    for id, interfac := range args.interfaces {
      if _, ok := sriovInterfaces[interfac.Network]; ok == true {
        args.interfaces[id].Device, sriovDevices = sriovDevices[len(sriovDevices)-1], sriovDevices[:len(sriovDevices)-1]
      }
    }
  }
  syncher := syncher.NewSyncher(len(args.interfaces))
  for nicID, nicParams := range args.interfaces {
    nicParams.DefaultIfaceName = "eth" + strconv.Itoa(nicID)
    go createInterface(syncher, nicParams, args)
  }
  err = syncher.GetAggregatedResult()
  return syncher.MergeCniResults(), err
}

func createInterface(syncher *syncher.Syncher, iface danmtypes.Interface, args *cniArgs) {
  danmClient, err := createDanmClient(args.stdIn)
  if err != nil {
    syncher.PushResult(iface.Network, err, nil)
    return
  }
  isDelegationRequired, netInfo, err := cnidel.IsDelegationRequired(danmClient, iface.Network, args.nameSpace)
  if err != nil {
    syncher.PushResult(iface.Network, err, nil)
    return
  }
  var cniRes *current.Result
  if isDelegationRequired {
    cniRes, err = createDelegatedInterface(danmClient, iface, netInfo, args)
    if err != nil {
      syncher.PushResult(iface.Network, err, nil)
      return
    }
  } else {
    cniRes, err = createDanmInterface(danmClient, iface, netInfo, args)
    if err != nil {
      syncher.PushResult(iface.Network, err, nil)
      return
    }
  }
  syncher.PushResult(iface.Network, nil, cniRes)
}

func createDelegatedInterface(danmClient danmclientset.Interface, iface danmtypes.Interface, netInfo *danmtypes.DanmNet, args *cniArgs) (*current.Result,error) {
  epIfaceSpec := danmtypes.DanmEpIface {
    Name:        cnidel.CalculateIfaceName(netInfo.Spec.Options.Prefix, iface.DefaultIfaceName),
    Address:     iface.Ip,
    AddressIPv6: iface.Ip6,
    Proutes:     iface.Proutes,
    Proutes6:    iface.Proutes6,
    VfDeviceID:  iface.Device,
  }
  ep, err := createDanmEp(epIfaceSpec, netInfo.Spec.NetworkID, netInfo.Spec.NetworkType, args)
  if err != nil {
    return nil, errors.New("DanmEp object could not be created due to error:" + err.Error())
  }
  delegatedResult,err := cnidel.DelegateInterfaceSetup(danmClient, netInfo, &ep)
  if err != nil {
    return nil, err
  }
  err = putDanmEp(args, ep)
  if err != nil {
    return nil, errors.New("DanmEp object could not be PUT to K8s due to error:" + err.Error())
  }
  return delegatedResult, nil
}

func createDanmInterface(danmClient danmclientset.Interface, iface danmtypes.Interface, netInfo *danmtypes.DanmNet, args *cniArgs) (*current.Result,error) {
  netId := netInfo.Spec.NetworkID
  ip4, ip6, macAddr, err := ipam.Reserve(danmClient, *netInfo, iface.Ip, iface.Ip6)
  if err != nil {
    return nil, errors.New("IP address reservation failed for network:" + netId + " with error:" + err.Error())
  }
  epSpec := danmtypes.DanmEpIface {
    Name: cnidel.CalculateIfaceName(netInfo.Spec.Options.Prefix, iface.DefaultIfaceName),
    Address: ip4,
    AddressIPv6: ip6,
    MacAddress: macAddr,
    Proutes: iface.Proutes,
    Proutes6: iface.Proutes6,
  }
  networkType := "ipvlan"
  ep, err := createDanmEp(epSpec, netId, networkType, args)
  if err != nil {
    ipam.GarbageCollectIps(danmClient, netInfo, ip4, ip6)
    return nil, errors.New("DanmEp object could not be created due to error:" + err.Error())
  }
  err = putDanmEp(args, ep)
  if err != nil {
    ipam.GarbageCollectIps(danmClient, netInfo, ip4, ip6)
    return nil, errors.New("EP could not be PUT into K8s due to error:" + err.Error())
  } 
  err = danmep.AddIpvlanInterface(netInfo, ep)
  if err != nil {
    ipam.GarbageCollectIps(danmClient, netInfo, ip4, ip6)
    deleteEp(danmClient, ep)
    return nil, errors.New("IPVLAN interface could not be created due to error:" + err.Error())
  } 
  danmResult := &current.Result{}
  addIfaceToResult(ep.Spec.EndpointID, epSpec.MacAddress, args.containerId, danmResult)
  if (ip4 != "") {
    addIpToResult(ip4,"4",danmResult)
  }
  if (ip6 != "") {
    addIpToResult(ip6,"6",danmResult)
  }
  return danmResult, nil
}

func createDanmEp(epInput danmtypes.DanmEpIface, netId string, neType string, args *cniArgs) (danmtypes.DanmEp, error) {
  epidInt, err := uuid.NewV4()
  if err != nil {
    return danmtypes.DanmEp{}, errors.New("uuid.NewV4 returned error during EP creation:" + err.Error())
  }
  epid := epidInt.String()
  host, err := os.Hostname()
  if err != nil {
    return danmtypes.DanmEp{}, errors.New("OS.Hostname returned error during EP creation:" + err.Error())
  }
  epSpec := danmtypes.DanmEpSpec {
    NetworkID: netId,
    NetworkType: neType,
    EndpointID: epid,
    Iface: epInput,
    Host: host,
    Pod: args.podId,
    CID: args.containerId,
    Netns: args.netns,
    Creator: "danm",
  }
  meta := meta_v1.ObjectMeta {
    Name: epid,
    Namespace: args.nameSpace,
    ResourceVersion: "",
    Labels: args.labels,
  }
  typeMeta := meta_v1.TypeMeta {
      APIVersion: danmtypes.SchemeGroupVersion.String(), 
      Kind: "DanmEp",
  }
  ep := danmtypes.DanmEp{
    TypeMeta: typeMeta,
    ObjectMeta: meta,
    Spec: epSpec, 
  }
  return ep, nil
}

func putDanmEp(args *cniArgs, ep danmtypes.DanmEp) error {
  danmClient, err := createDanmClient(args.stdIn)
  if err != nil {
    return err
  }
  _, err = danmClient.DanmV1().DanmEps(ep.Namespace).Create(&ep)
  if err != nil {
    return err
  }
  return nil
}

func addIfaceToResult(epid string, macAddress string, sandBox string, cniResult *current.Result) {
  iface := &current.Interface{
    Name: epid,
    Mac: macAddress,
    Sandbox: sandBox,
  }
  cniResult.Interfaces = append(cniResult.Interfaces, iface)
}

func addIpToResult(ip string, version string, cniResult *current.Result) {
  if ip != "" {
    ip, _ := types.ParseCIDR(ip)
    ipConf := &current.IPConfig {
      Version: version,
      Address: *ip,
    }
    cniResult.IPs = append(cniResult.IPs, ipConf)
  }
}

func deleteInterfaces(args *skel.CmdArgs) error {
  cniArgs,err := extractCniArgs(args)
  log.Println("CNI DEL invoked with: ns:" + cniArgs.nameSpace + " PID:" + cniArgs.podId + " CID: " + cniArgs.containerId)
  if err != nil {
    log.Println("INFO: DEL: CNI args could not be loaded because" + err.Error())
    return nil
  }
  danmClient, err := createDanmClient(cniArgs.stdIn)
  if err != nil {
    log.Println("INFO: DEL: DanmEp REST client could not be created because" + err.Error())
    return nil
  }
  eplist, err := danmep.FindByCid(danmClient, cniArgs.containerId)
  if err != nil {
    log.Println("INFO: DEL: Could not interrogate DanmEps from K8s API server because" + err.Error())
    return nil
  }
  syncher := syncher.NewSyncher(len(eplist))
  for _, ep := range eplist {
    go deleteInterface(cniArgs, syncher, ep)
  }
  deleteErrors := syncher.GetAggregatedResult()
  if deleteErrors != nil {
    log.Println("INFO: DEL: Following errors happened during interface deletion:" + deleteErrors.Error())
  }
  return nil
}

func deleteInterface(args *cniArgs, syncher *syncher.Syncher, ep danmtypes.DanmEp) {
  danmClient, err := createDanmClient(args.stdIn)
  if err != nil {
    syncher.PushResult(ep.Spec.NetworkID, errors.New("failed to create danmClient:" + err.Error()), nil)
    return
  }
  netInfo, err := danmClient.DanmV1().DanmNets(args.nameSpace).Get(ep.Spec.NetworkID, meta_v1.GetOptions{})
  if err != nil {
    syncher.PushResult(ep.Spec.NetworkID, errors.New("failed to get DanmNet:"+ err.Error()), nil)
    return
  }
  var aggregatedError string
  err = deleteNic(danmClient, netInfo, ep)
  //It can happen that a container was already destroyed at this point in this fully asynch world
  //So we are not interested in errors, but we also can't just return yet, we need to try and clean-up remaining resources, if, any
  if err != nil {
    aggregatedError += "failed to delete container NIC:" + err.Error() + "; "
  }
  err = deleteEp(danmClient, ep)
  if err != nil {
    aggregatedError += "failed to delete DanmEp:" + err.Error() + "; "
  }
  if aggregatedError != "" {
    syncher.PushResult(ep.Spec.NetworkID, errors.New(aggregatedError), nil)
  } else {
    syncher.PushResult(ep.Spec.NetworkID, nil, nil)
  }
}

func deleteNic(danmClient danmclientset.Interface, netInfo *danmtypes.DanmNet, ep danmtypes.DanmEp) error {
  var err error
  if ep.Spec.NetworkType != "ipvlan" {
    err = cnidel.DelegateInterfaceDelete(danmClient, netInfo, &ep)
  } else {
    err = deleteDanmNet(danmClient, ep, netInfo)
  }
  return err
}

func deleteEp(danmClient danmclientset.Interface, ep danmtypes.DanmEp) error {
  delOpts := meta_v1.DeleteOptions{}
  err := danmClient.DanmV1().DanmEps(ep.ObjectMeta.Namespace).Delete(ep.ObjectMeta.Name, &delOpts)
  if err != nil {
    return err
  }
  return nil
}

func deleteDanmNet(danmClient danmclientset.Interface, ep danmtypes.DanmEp, netInfo *danmtypes.DanmNet) error {
  ipam.GarbageCollectIps(danmClient, netInfo, ep.Spec.Iface.Address, ep.Spec.Iface.AddressIPv6)
  return danmep.DeleteIpvlanInterface(ep)
}

func main() {
  var err error
  f, err := os.OpenFile("/var/log/plugin.log", os.O_RDWR | os.O_CREATE | os.O_APPEND, 0640)
  if err == nil {
    log.SetOutput(f)
    defer f.Close()
  }
  skel.PluginMain(createInterfaces, deleteInterfaces, version.All)
}
