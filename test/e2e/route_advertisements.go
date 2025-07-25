package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"

	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	rav1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	raclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1/apis/clientset/versioned"
	apitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/types"
	udnv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	udnclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1/apis/clientset/versioned"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("BGP: Pod to external server when default podNetwork is advertised", func() {
	const (
		serverContainerName    = "bgpserver"
		routerContainerName    = "frr"
		echoClientPodName      = "echo-client-pod"
		bgpExternalNetworkName = "bgpnet"
	)
	var serverContainerIPs []string
	var frrContainerIPv4, frrContainerIPv6 string
	var nodes *corev1.NodeList
	f := wrappedTestFramework("pod2external-route-advertisements")

	ginkgo.BeforeEach(func() {
		serverContainerIPs = []string{}
		bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
		framework.ExpectNoError(err, "must get bgpnet network")
		bgpServer := infraapi.ExternalContainer{Name: serverContainerName}
		networkInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(bgpServer, bgpNetwork)
		framework.ExpectNoError(err, "container %s attached to network %s must contain network info", serverContainerName, bgpExternalNetworkName)
		if isIPv4Supported() && len(networkInterface.IPv4) > 0 {
			serverContainerIPs = append(serverContainerIPs, networkInterface.IPv4)
		}
		if isIPv6Supported() && len(networkInterface.IPv6) > 0 {
			serverContainerIPs = append(serverContainerIPs, networkInterface.IPv6)
		}
		framework.Logf("The external server IPs are: %+v", serverContainerIPs)
		providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "provider primary network must be available")
		externalContainerNetInf, err := infraprovider.Get().GetExternalContainerNetworkInterface(infraapi.ExternalContainer{Name: routerContainerName}, providerPrimaryNetwork)
		framework.ExpectNoError(err, "external container %s network %s information must be available", routerContainerName, providerPrimaryNetwork.Name())
		frrContainerIPv4, frrContainerIPv6 = externalContainerNetInf.IPv4, externalContainerNetInf.IPv6
		framework.Logf("The frr router container IPs are: %s/%s", frrContainerIPv4, frrContainerIPv6)
	})

	ginkgo.When("a client ovnk pod targeting an external server is created", func() {

		var clientPod *corev1.Pod
		var clientPodNodeName string
		var err error

		ginkgo.BeforeEach(func() {
			if !isDefaultNetworkAdvertised() {
				e2eskipper.Skipf(
					"skipping pod to external server tests when podNetwork is not advertised",
				)
			}
			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))

			ginkgo.By("Selecting node for client pod")
			clientPodNodeName = nodes.Items[1].Name

			ginkgo.By("Creating client pod")
			clientPod = e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			clientPod.Spec.NodeName = clientPodNodeName
			for k := range clientPod.Spec.Containers {
				if clientPod.Spec.Containers[k].Name == "agnhost-container" {
					clientPod.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
			}
			e2epod.NewPodClient(f).CreateSync(context.TODO(), clientPod)

			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))
		})
		// -----------------               ------------------                         ---------------------
		// |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
		// |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
		// |    server     |               |                |                         |    ovn-worker     |   (client pod advertised
		// -----------------               ------------------                         ---------------------    using RouteAdvertisements
		//                                                                            |    ovn-worker2    |    from default pod network)
		//                                                                            ---------------------
		// The client pod inside the KIND cluster on the default network exposed using default network Router
		// Advertisement will curl the external server container sitting outside the cluster via a FRR router
		// This test ensures the north-south connectivity is happening through podIP
		ginkgo.It("tests are run towards the external agnhost echo server", func() {
			ginkgo.By("routes from external bgp server are imported by nodes in the cluster")
			bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
			framework.ExpectNoError(err, "network %s must be available and precreated before test run", bgpExternalNetworkName)
			externalServerV4CIDR, externalServerV6CIDR, err := bgpNetwork.IPv4IPv6Subnets()
			framework.ExpectNoError(err, "must get bgpnet subnets")
			framework.Logf("the network cidrs to be imported are v4=%s and v6=%s", externalServerV4CIDR, externalServerV6CIDR)
			for _, node := range nodes.Items {
				ipVer := ""
				bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV4CIDR), " ")
				framework.Logf("Checking for server's route in node %s", node.Name)
				gomega.Eventually(func() bool {
					routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
					framework.ExpectNoError(err, "failed to get BGP routes from node")
					framework.Logf("Routes in node %s", routes)
					return strings.Contains(routes, frrContainerIPv4)
				}, 30*time.Second).Should(gomega.BeTrue())
				if isDualStackCluster(nodes) {
					ipVer = " -6"
					nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(routerContainerName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV6CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, nodeIPv6LLA)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("routes to the default pod network are advertised to external frr router")
			// Get the first element in the advertisements array (assuming you want to check the first one)
			gomega.Eventually(func() string {
				podNetworkValue, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "--template={{index .spec.advertisements 0}}")
				if err != nil {
					return ""
				}
				return podNetworkValue
			}, 5*time.Second, time.Second).Should(gomega.Equal("PodNetwork"))

			gomega.Eventually(func() string {
				reason, err := e2ekubectl.RunKubectl("", "get", "ra", "default", "-o", "jsonpath={.status.conditions[?(@.type=='Accepted')].reason}")
				if err != nil {
					return ""
				}
				return reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			ginkgo.By("all 3 node's podSubnet routes are exported correctly to external FRR router by frr-k8s speakers")
			// sample
			//10.244.0.0/24 nhid 27 via 172.18.0.3 dev eth0 proto bgp metric 20
			//10.244.1.0/24 nhid 30 via 172.18.0.2 dev eth0 proto bgp metric 20
			//10.244.2.0/24 nhid 25 via 172.18.0.4 dev eth0 proto bgp metric 20
			for _, serverContainerIP := range serverContainerIPs {
				for _, node := range nodes.Items {
					podv4CIDR, podv6CIDR, err := getNodePodCIDRs(node.Name)
					if err != nil {
						framework.Failf("Error retrieving the pod cidr from %s %v", node.Name, err)
					}
					framework.Logf("the pod cidr for node %s-%s is %s", node.Name, podv4CIDR, podv6CIDR)
					ipVer := ""
					podCIDR := podv4CIDR
					nodeIP := e2enode.GetAddressesByTypeAndFamily(&node, corev1.NodeInternalIP, corev1.IPv4Protocol)
					if utilnet.IsIPv6String(serverContainerIP) {
						ipVer = " -6"
						podCIDR = podv6CIDR
						// BGP by default uses LLA as nexthops in its routes
						nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(node.Name)
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
						nodeIP = []string{nodeIPv6LLA}
					}
					gomega.Expect(len(nodeIP)).To(gomega.BeNumerically(">", 0))
					framework.Logf("the nodeIP for node %s is %+v", node.Name, nodeIP)
					externalContainer := infraapi.ExternalContainer{Name: routerContainerName}
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, podCIDR), " ")
					framework.Logf("Checking for node %s's route for pod subnet %s", node.Name, podCIDR)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from intermediary router")
						framework.Logf("Routes in FRR %s", routes)
						return strings.Contains(routes, nodeIP[0])
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("queries to the external server are not SNATed (uses podIP)")
			podv4IP, podv6IP, err := podIPsForDefaultNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name)
			framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
			framework.Logf("Client pod IP address v4=%s, v6=%s", podv4IP, podv6IP)
			for _, serverContainerIP := range serverContainerIPs {
				ginkgo.By(fmt.Sprintf("Sending request to node IP %s "+
					"and expecting to receive the same payload", serverContainerIP))
				cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s/clientip",
					net.JoinHostPort(serverContainerIP, "8080"),
				)
				framework.Logf("Testing pod to external traffic with command %q", cmd)
				stdout, err := e2epodoutput.RunHostCmdWithRetries(
					clientPod.Namespace,
					clientPod.Name,
					cmd,
					framework.Poll,
					60*time.Second)
				framework.ExpectNoError(err, fmt.Sprintf("Testing pod to external traffic failed: %v", err))
				expectedPodIP := podv4IP
				if isIPv6Supported() && utilnet.IsIPv6String(serverContainerIP) {
					expectedPodIP = podv6IP
					// For IPv6 addresses, need to handle the brackets in the output
					outputIP := strings.TrimPrefix(strings.Split(stdout, "]:")[0], "[")
					gomega.Expect(outputIP).To(gomega.Equal(expectedPodIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				} else {
					// Original IPv4 handling
					gomega.Expect(strings.Split(stdout, ":")[0]).To(gomega.Equal(expectedPodIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				}
			}
		})
	})
})

var _ = ginkgo.Describe("BGP: Pod to external server when CUDN network is advertised", func() {
	const (
		serverContainerName    = "bgpserver"
		routerContainerName    = "frr"
		echoClientPodName      = "echo-client-pod"
		bgpExternalNetworkName = "bgpnet"
		placeholder            = "PLACEHOLDER_NAMESPACE"
	)
	var serverContainerIPs []string
	var frrContainerIPv4, frrContainerIPv6 string
	var nodes *corev1.NodeList
	var clientPod *corev1.Pod

	f := wrappedTestFramework("pod2external-route-advertisements")
	f.SkipNamespaceCreation = true

	ginkgo.BeforeEach(func() {
		var err error
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		f.Namespace = namespace

		serverContainerIPs = []string{}

		bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName) // pre-created network
		framework.ExpectNoError(err, "must get bgpnet network")
		bgpServer := infraapi.ExternalContainer{Name: serverContainerName}
		networkInterface, err := infraprovider.Get().GetExternalContainerNetworkInterface(bgpServer, bgpNetwork)
		framework.ExpectNoError(err, "container %s attached to network %s must contain network info", serverContainerName, bgpExternalNetworkName)
		if isIPv4Supported() && len(networkInterface.IPv4) > 0 {
			serverContainerIPs = append(serverContainerIPs, networkInterface.IPv4)
		}
		if isIPv6Supported() && len(networkInterface.IPv6) > 0 {
			serverContainerIPs = append(serverContainerIPs, networkInterface.IPv6)
		}
		gomega.Expect(len(serverContainerIPs)).Should(gomega.BeNumerically(">", 0), "failed to find external container IPs")
		framework.Logf("The external server IPs are: %+v", serverContainerIPs)
		providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "provider primary network must be available")
		frrContainer := infraapi.ExternalContainer{Name: routerContainerName}
		networkInterface, err = infraprovider.Get().GetExternalContainerNetworkInterface(frrContainer, providerPrimaryNetwork)
		framework.ExpectNoError(err, "container %s attached to network %s must contain network info", routerContainerName, providerPrimaryNetwork.Name())
		frrContainerIPv4, frrContainerIPv6 = networkInterface.IPv4, networkInterface.IPv6
		framework.Logf("The frr router container IPs are: %s/%s", frrContainerIPv4, frrContainerIPv6)

		// Select nodes here so they're available for all tests
		ginkgo.By("Selecting 3 schedulable nodes")
		nodes, err = e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))
	})

	ginkgo.DescribeTable("Route Advertisements",
		func(cudnTemplate *udnv1.ClusterUserDefinedNetwork, ra *rav1.RouteAdvertisements) {
			// set the exact selector
			cudnTemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{f.Namespace.Name},
			}}}

			if IsGatewayModeLocal() && cudnTemplate.Spec.Network.Topology == udnv1.NetworkTopologyLayer2 {
				e2eskipper.Skipf(
					"BGP for L2 networks on LGW is currently unsupported",
				)
			}
			// Create CUDN
			ginkgo.By("create ClusterUserDefinedNetwork")
			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			cUDN, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnTemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() {
				udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cUDN.Name, metav1.DeleteOptions{})
			})
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cUDN.Name), 5*time.Second, time.Second).Should(gomega.Succeed())

			ginkgo.DeferCleanup(func() {
				ginkgo.By(fmt.Sprintf("delete pods in %s namespace to unblock CUDN CR & associate NAD deletion", f.Namespace.Name))
				gomega.Expect(f.ClientSet.CoreV1().Pods(f.Namespace.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
			})

			// Create client pod
			ginkgo.By("Creating client pod")
			podSpec := e2epod.NewAgnhostPod(f.Namespace.Name, echoClientPodName, nil, nil, nil)
			podSpec.Spec.NodeName = nodes.Items[1].Name
			for k := range podSpec.Spec.Containers {
				if podSpec.Spec.Containers[k].Name == "agnhost-container" {
					podSpec.Spec.Containers[k].Command = []string{
						"sleep",
						"infinity",
					}
				}
			}
			clientPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), podSpec)

			// Create route advertisement
			ginkgo.By("create router advertisement")
			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ra, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			ginkgo.DeferCleanup(func() { raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), ra.Name, metav1.DeleteOptions{}) })
			ginkgo.By("ensure route advertisement matching CUDN was created successfully")
			gomega.Eventually(func() string {
				ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ra.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				if condition == nil {
					return ""
				}
				return condition.Reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))

			gomega.Expect(len(serverContainerIPs)).To(gomega.BeNumerically(">", 0))

			// -----------------               ------------------                         ---------------------
			// |               | 172.26.0.0/16 |                |       172.18.0.0/16     | ovn-control-plane |
			// |   external    |<------------- |   FRR router   |<------ KIND cluster --  ---------------------
			// |    server     |               |                |                         |    ovn-worker     |   (client UDN pod advertised
			// -----------------               ------------------                         ---------------------    using RouteAdvertisements
			//                                                                            |    ovn-worker2    |    from default pod network)
			//                                                                            ---------------------
			// The client pod inside the KIND cluster on the default network exposed using default network Router
			// Advertisement will curl the external server container sitting outside the cluster via a FRR router
			// This test ensures the north-south connectivity is happening through podIP
			ginkgo.By("routes from external bgp server are imported by nodes in the cluster")
			bgpNetwork, err := infraprovider.Get().GetNetwork(bgpExternalNetworkName)
			framework.ExpectNoError(err, "network %s must be available", bgpExternalNetworkName)
			externalServerV4CIDR, externalServerV6CIDR, err := bgpNetwork.IPv4IPv6Subnets()
			framework.ExpectNoError(err, "must get BGP network subnets")
			framework.Logf("the network cidrs to be imported are v4=%s and v6=%s", externalServerV4CIDR, externalServerV6CIDR)
			for _, node := range nodes.Items {
				ipVer := ""
				bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV4CIDR), " ")
				framework.Logf("Checking for server's route in node %s", node.Name)
				gomega.Eventually(func() bool {
					routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
					framework.ExpectNoError(err, "failed to get BGP routes from node")
					framework.Logf("Routes in node %s", routes)
					return strings.Contains(routes, frrContainerIPv4)
				}, 30*time.Second).Should(gomega.BeTrue())
				if isDualStackCluster(nodes) {
					ipVer = " -6"
					nodeIPv6LLA, err := GetNodeIPv6LinkLocalAddressForEth0(routerContainerName)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					bgpRouteCommand := strings.Split(fmt.Sprintf("ip%s route show %s", ipVer, externalServerV6CIDR), " ")
					framework.Logf("Checking for server's route in node %s", node.Name)
					gomega.Eventually(func() bool {
						routes, err := infraprovider.Get().ExecK8NodeCommand(node.GetName(), bgpRouteCommand)
						framework.ExpectNoError(err, "failed to get BGP routes from node")
						framework.Logf("Routes in node %s", routes)
						return strings.Contains(routes, nodeIPv6LLA)
					}, 30*time.Second).Should(gomega.BeTrue())
				}
			}

			ginkgo.By("queries to the external server are not SNATed (uses podIP)")
			for _, serverContainerIP := range serverContainerIPs {
				podIP, err := podIPsForUserDefinedPrimaryNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name, namespacedName(f.Namespace.Name, cUDN.Name), 0)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				framework.ExpectNoError(err, fmt.Sprintf("Getting podIPs for pod %s failed: %v", clientPod.Name, err))
				framework.Logf("Client pod IP address=%s", podIP)

				ginkgo.By(fmt.Sprintf("Sending request to node IP %s "+
					"and expecting to receive the same payload", serverContainerIP))
				cmd := fmt.Sprintf("curl --max-time 10 -g -q -s http://%s/clientip",
					net.JoinHostPort(serverContainerIP, "8080"),
				)
				framework.Logf("Testing pod to external traffic with command %q", cmd)
				stdout, err := e2epodoutput.RunHostCmdWithRetries(
					clientPod.Namespace,
					clientPod.Name,
					cmd,
					framework.Poll,
					60*time.Second)
				framework.ExpectNoError(err, fmt.Sprintf("Testing pod to external traffic failed: %v", err))
				if isIPv6Supported() && utilnet.IsIPv6String(serverContainerIP) {
					podIP, err = podIPsForUserDefinedPrimaryNetwork(f.ClientSet, f.Namespace.Name, clientPod.Name, namespacedName(f.Namespace.Name, cUDN.Name), 1)
					// For IPv6 addresses, need to handle the brackets in the output
					outputIP := strings.TrimPrefix(strings.Split(stdout, "]:")[0], "[")
					gomega.Expect(outputIP).To(gomega.Equal(podIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				} else {
					// Original IPv4 handling
					gomega.Expect(strings.Split(stdout, ":")[0]).To(gomega.Equal(podIP),
						fmt.Sprintf("Testing pod %s to external traffic failed while analysing output %v", echoClientPodName, stdout))
				}
			}
		},
		ginkgo.Entry("layer3",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-udn-layer3-network",
					Labels:       map[string]string{"bgp-udn-layer3-network": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer3,
						Layer3: &udnv1.Layer3Config{
							Role: "Primary",
							Subnets: generateL3Subnets(udnv1.Layer3Subnet{
								CIDR:       "103.103.0.0/16",
								HostSubnet: 24,
							}, udnv1.Layer3Subnet{
								CIDR:       "2014:100:200::0/60",
								HostSubnet: 64,
							}),
						},
					},
				},
			},
			&rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bgp-udn-layer3-network-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"bgp-udn-layer3-network": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			},
		),
		ginkgo.Entry("layer2",
			&udnv1.ClusterUserDefinedNetwork{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "bgp-udn-layer2-network",
					Labels:       map[string]string{"bgp-udn-layer2-network": ""},
				},
				Spec: udnv1.ClusterUserDefinedNetworkSpec{
					Network: udnv1.NetworkSpec{
						Topology: udnv1.NetworkTopologyLayer2,
						Layer2: &udnv1.Layer2Config{
							Role:    "Primary",
							Subnets: generateL2Subnets("103.0.0.0/16", "2014:100::0/60"),
						},
					},
				},
			},
			&rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					Name: "bgp-udn-layer2-network-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"bgp-udn-layer2-network": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			},
		),
	)
})

var _ = ginkgo.DescribeTableSubtree("BGP: isolation between advertised networks",
	func(cudnATemplate, cudnBTemplate *udnv1.ClusterUserDefinedNetwork) {
		const curlConnectionTimeoutCode = "28"

		f := wrappedTestFramework("bpp-network-isolation")
		f.SkipNamespaceCreation = true
		var udnNamespaceA, udnNamespaceB *corev1.Namespace
		var nodes *corev1.NodeList
		// podsNetA has 3 pods in cudnA, two are on nodes[0] and the last one is on nodes[1] - done in BeforeEach
		var podsNetA []*corev1.Pod

		// podNetB is in cudnB hosted on nodes[1], podNetDefault is in the default network hosted on nodes[1] - done in BeforeEach
		var podNetB, podNetDefault *corev1.Pod
		var svcNetA, svcNetB, svcNetDefault *corev1.Service
		var cudnA, cudnB *udnv1.ClusterUserDefinedNetwork
		var ra *rav1.RouteAdvertisements
		var hostNetworkPort int
		ginkgo.BeforeEach(func() {
			if cudnATemplate.Spec.Network.Topology == udnv1.NetworkTopologyLayer2 && isLocalGWModeEnabled() {
				e2eskipper.Skipf("Advertising Layer2 UDNs is not currently supported in LGW")
			}
			ginkgo.By("Configuring primary UDN namespaces")
			var err error
			udnNamespaceA, err = f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			f.Namespace = udnNamespaceA
			udnNamespaceB, err = f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Configuring networks")
			cudnATemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{udnNamespaceA.Name},
			}}}
			cudnBTemplate.Spec.NamespaceSelector = metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{udnNamespaceB.Name},
			}}}

			// set a common label used to advertise both networks with one RA
			cudnATemplate.Labels["advertised-networks-isolation"] = ""
			cudnBTemplate.Labels["advertised-networks-isolation"] = ""

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cudnA, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnATemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			cudnB, err = udnClient.K8sV1().ClusterUserDefinedNetworks().Create(context.Background(), cudnBTemplate, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Waiting for networks to be ready")
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnA.Name), 5*time.Second, time.Second).Should(gomega.Succeed())
			gomega.Eventually(clusterUserDefinedNetworkReadyFunc(f.DynamicClient, cudnB.Name), 5*time.Second, time.Second).Should(gomega.Succeed())

			ginkgo.By("Selecting 3 schedulable nodes")
			nodes, err = e2enode.GetReadySchedulableNodes(context.TODO(), f.ClientSet)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(len(nodes.Items)).To(gomega.BeNumerically(">", 2))
			// create host networked pod
			ginkgo.By("Creating host network pods on each node")
			// get random port in case the test retries and port is already in use on host node
			min := 25000
			max := 25999
			hostNetworkPort = rand.Intn(max-min+1) + min
			framework.Logf("Random host networked port chosen: %d", hostNetworkPort)
			for _, node := range nodes.Items {
				// this creates a udp / http netexec listener which is able to receive the "hostname"
				// command. We use this to validate that each endpoint is received at least once
				args := []string{
					"netexec",
					fmt.Sprintf("--http-port=%d", hostNetworkPort),
					fmt.Sprintf("--udp-port=%d", hostNetworkPort),
				}

				// create host networked Pods
				_, err := createPod(f, node.Name+"-hostnet-ep", node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
					p.Spec.Containers[0].Args = args
					p.Spec.HostNetwork = true
				})

				framework.ExpectNoError(err)
			}

			ginkgo.By("Setting up pods and services")
			podsNetA = []*corev1.Pod{}
			pod := e2epod.NewAgnhostPod(udnNamespaceA.Name, fmt.Sprintf("pod-1-%s-net-%s", nodes.Items[0].Name, cudnA.Name), nil, nil, []corev1.ContainerPort{{ContainerPort: 8080}}, "netexec")
			pod.Spec.NodeName = nodes.Items[0].Name
			pod.Labels = map[string]string{"network": cudnA.Name}
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			pod.Name = fmt.Sprintf("pod-2-%s-net-%s", nodes.Items[0].Name, cudnA.Name)
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			pod.Name = fmt.Sprintf("pod-3-%s-net-%s", nodes.Items[1].Name, cudnA.Name)
			pod.Spec.NodeName = nodes.Items[1].Name
			podsNetA = append(podsNetA, e2epod.NewPodClient(f).CreateSync(context.TODO(), pod))

			svc := e2eservice.CreateServiceSpec(fmt.Sprintf("service-%s", cudnA.Name), "", false, pod.Labels)
			svc.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
			familyPolicy := corev1.IPFamilyPolicyPreferDualStack
			svc.Spec.IPFamilyPolicy = &familyPolicy
			svcNetA, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			pod.Name = fmt.Sprintf("pod-1-%s-net-%s", nodes.Items[1].Name, cudnB.Name)
			pod.Namespace = udnNamespaceB.Name
			pod.Labels = map[string]string{"network": cudnB.Name}
			podNetB = e2epod.PodClientNS(f, udnNamespaceB.Name).CreateSync(context.TODO(), pod)
			framework.Logf("created pod %s/%s", podNetB.Namespace, podNetB.Name)

			svc.Name = fmt.Sprintf("service-%s", cudnB.Name)
			svc.Namespace = pod.Namespace
			svc.Spec.Selector = pod.Labels
			svcNetB, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			pod.Name = fmt.Sprintf("pod-1-%s-net-default", nodes.Items[1].Name)
			pod.Namespace = "default"
			pod.Labels = map[string]string{"network": "default"}
			podNetDefault = e2epod.PodClientNS(f, "default").CreateSync(context.TODO(), pod)

			svc.Name = fmt.Sprintf("service-default")
			svc.Namespace = "default"
			svc.Spec.Selector = pod.Labels
			svcNetDefault, err = f.ClientSet.CoreV1().Services(pod.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("Expose networks")
			ra = &rav1.RouteAdvertisements{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "advertised-networks-isolation-ra",
				},
				Spec: rav1.RouteAdvertisementsSpec{
					NetworkSelectors: apitypes.NetworkSelectors{
						apitypes.NetworkSelector{
							NetworkSelectionType: apitypes.ClusterUserDefinedNetworks,
							ClusterUserDefinedNetworkSelector: &apitypes.ClusterUserDefinedNetworkSelector{
								NetworkSelector: metav1.LabelSelector{
									MatchLabels: map[string]string{"advertised-networks-isolation": ""},
								},
							},
						},
					},
					NodeSelector:             metav1.LabelSelector{},
					FRRConfigurationSelector: metav1.LabelSelector{},
					Advertisements: []rav1.AdvertisementType{
						rav1.PodNetwork,
					},
				},
			}

			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ra, err = raClient.K8sV1().RouteAdvertisements().Create(context.TODO(), ra, metav1.CreateOptions{})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			ginkgo.By("ensure route advertisement matching both networks was created successfully")
			gomega.Eventually(func() string {
				ra, err := raClient.K8sV1().RouteAdvertisements().Get(context.TODO(), ra.Name, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				condition := meta.FindStatusCondition(ra.Status.Conditions, "Accepted")
				if condition == nil {
					return ""
				}
				return condition.Reason
			}, 30*time.Second, time.Second).Should(gomega.Equal("Accepted"))
		})

		ginkgo.AfterEach(func() {
			if cudnATemplate.Spec.Network.Topology == udnv1.NetworkTopologyLayer2 && isLocalGWModeEnabled() {
				return
			}
			gomega.Expect(f.ClientSet.CoreV1().Pods(udnNamespaceA.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())
			gomega.Expect(f.ClientSet.CoreV1().Pods(udnNamespaceB.Name).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{})).To(gomega.Succeed())

			udnClient, err := udnclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			if cudnB != nil {
				err = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnB.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() bool {
					_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnB.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, time.Second*30).Should(gomega.BeTrue())
				cudnB = nil
			}
			if cudnA != nil {
				err = udnClient.K8sV1().ClusterUserDefinedNetworks().Delete(context.TODO(), cudnA.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() bool {
					_, err := udnClient.K8sV1().ClusterUserDefinedNetworks().Get(context.TODO(), cudnA.Name, metav1.GetOptions{})
					return apierrors.IsNotFound(err)
				}, time.Second*30).Should(gomega.BeTrue())
				cudnA = nil
			}

			if podNetDefault != nil {
				err = f.ClientSet.CoreV1().Pods(podNetDefault.Namespace).Delete(context.Background(), podNetDefault.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				podNetDefault = nil
			}
			if svcNetDefault != nil {
				err = f.ClientSet.CoreV1().Services(svcNetDefault.Namespace).Delete(context.Background(), svcNetDefault.Name, metav1.DeleteOptions{})
				svcNetDefault = nil
			}

			raClient, err := raclientset.NewForConfig(f.ClientConfig())
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

			if ra != nil {
				err = raClient.K8sV1().RouteAdvertisements().Delete(context.TODO(), ra.Name, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				ra = nil
			}
		})

		ginkgo.DescribeTable("connectivity between networks",
			func(connInfo func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool)) {
				// checkConnectivity performs a curl command from a specified client (pod or node)
				// to targetAddress. If clientNamespace is empty the function assumes clientName is a node that will be used as the
				// client.
				var checkConnectivity = func(clientName, clientNamespace, targetAddress string) (string, error) {
					curlCmd := []string{"curl", "-g", "-q", "-s", "--max-time", "2", "--insecure", targetAddress}
					var out string
					var err error
					if clientNamespace != "" {
						framework.Logf("Attempting connectivity from pod: %s/%s -> %s", clientNamespace, clientName, targetAddress)
						stdout, stderr, err := e2epodoutput.RunHostCmdWithFullOutput(clientNamespace, clientName, strings.Join(curlCmd, " "))
						out = stdout + "\n" + stderr
						if err != nil {
							return out, fmt.Errorf("connectivity check failed from Pod %s/%s to %s: %w", clientNamespace, clientName, targetAddress, err)
						}
					} else {
						framework.Logf("Attempting connectivity from node: %s -> %s", clientName, targetAddress)
						out, err = infraprovider.Get().ExecK8NodeCommand(clientName, curlCmd)
						if err != nil {
							// out is empty on error and error contains out...
							return err.Error(), fmt.Errorf("connectivity check failed from node %s to %s: %w", clientName, targetAddress, err)
						}
					}

					client := clientName
					if clientNamespace != "" {
						client = clientNamespace + "/" + client
					}
					framework.Logf("Connectivity check successful:'%s' -> %s", client, targetAddress)
					return out, nil
				}
				clientName, clientNamespace, dst, expectedOutput, expectErr := connInfo(0)

				asyncAssertion := gomega.Eventually
				timeout := time.Second * 30
				if expectErr {
					// When the connectivity check is expected to fail it should be failing consistently
					asyncAssertion = gomega.Consistently
					timeout = time.Second * 15
				}
				asyncAssertion(func() error {
					out, err := checkConnectivity(clientName, clientNamespace, dst)
					if expectErr != (err != nil) {
						return fmt.Errorf("expected connectivity check to return error(%t), got %v, output %v", expectErr, err, out)
					}
					if expectedOutput != "" {
						if !strings.Contains(out, expectedOutput) {
							return fmt.Errorf("expected connectivity check to contain %q, got %q", expectedOutput, out)
						}
					}
					if isIPv6Supported() && isIPv4Supported() {
						// use ipFamilyIndex of 1 to pick the IPv6 addresses
						clientName, clientNamespace, dst, expectedOutput, expectErr := connInfo(1)
						out, err := checkConnectivity(clientName, clientNamespace, dst)
						if expectErr != (err != nil) {
							return fmt.Errorf("expected connectivity check to return error(%t), got %v, output %v", expectErr, err, out)
						}
						if expectedOutput != "" {
							if !strings.Contains(out, expectedOutput) {
								return fmt.Errorf("expected connectivity check to contain %q, got %q", expectedOutput, out)
							}
						}
					}
					return nil
				}, timeout).Should(gomega.BeNil())
			},
			ginkgo.Entry("pod to pod on the same network and same node should work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[1] are on the same node
					clientPod := podsNetA[0]
					srvPod := podsNetA[1]

					clientPodStatus, err := userDefinedNetworkStatus(clientPod, namespacedName(clientPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", clientPodStatus.IPs[ipFamilyIndex].IP.String(), false
				}),
			ginkgo.Entry("pod to pod on the same network and different nodes should work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[2] are on different nodes
					clientPod := podsNetA[0]
					srvPod := podsNetA[2]

					clientPodStatus, err := userDefinedNetworkStatus(clientPod, namespacedName(clientPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", clientPodStatus.IPs[ipFamilyIndex].IP.String(), false
				}),
			ginkgo.Entry("pod to pod on different networks and same node should not work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[2] and podNetB are on the same node
					clientPod := podsNetA[2]
					srvPod := podNetB

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnBTemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),

			ginkgo.Entry("pod to pod on different networks and different nodes should not work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podNetB are on different nodes
					clientPod := podsNetA[0]
					srvPod := podNetB

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnBTemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("pod in the default network should not be able to access an advertised UDN pod on the same node",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podNetDefault and podNetB are on the same node
					clientPod := podNetDefault
					srvPod := podNetB

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnBTemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("pod in the default network should not be able to access an advertised UDN pod on a different node",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podNetDefault and podsNetA[0] are on different nodes
					clientPod := podNetDefault
					srvPod := podsNetA[0]

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("pod in the default network should not be able to access a UDN service",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podNetDefault.Name, podNetDefault.Namespace, net.JoinHostPort(svcNetA.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("pod in the UDN should be able to access a service in the same network",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetA.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", "", false
				}),
			ginkgo.Entry("pod in the UDN should not be able to access a default network service",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					err := true
					out := curlConnectionTimeoutCode
					if cudnATemplate.Spec.Network.Topology == udnv1.NetworkTopologyLayer2 {
						// FIXME: prevent looping of traffic in L2 UDNs
						// bad behaviour: packet is looping from management port -> breth0 -> GR -> management port -> breth0 and so on
						// which is a never ending loop
						// this causes curl timeout with code 7 host unreachable instead of code 28
						out = ""
					}
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetDefault.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", out, err
				}),
			ginkgo.Entry("pod in the UDN should be able to access kapi in default network service",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podsNetA[0].Name, podsNetA[0].Namespace, "https://kubernetes.default/healthz", "", false
				}),
			ginkgo.Entry("pod in the UDN should not be able to access a service in a different UDN",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					return podsNetA[0].Name, podsNetA[0].Namespace, net.JoinHostPort(svcNetB.Spec.ClusterIPs[ipFamilyIndex], "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("host to a local UDN pod should not work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					clientNode := podsNetA[0].Spec.NodeName
					srvPod := podsNetA[0]

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return clientNode, "", net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("host to a different node UDN pod should not work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					// podsNetA[0] and podsNetA[2] are on different nodes
					clientNode := podsNetA[2].Spec.NodeName
					srvPod := podsNetA[0]

					srvPodStatus, err := userDefinedNetworkStatus(srvPod, namespacedName(srvPod.Namespace, cudnATemplate.Name))
					framework.ExpectNoError(err)
					return clientNode, "", net.JoinHostPort(srvPodStatus.IPs[ipFamilyIndex].IP.String(), "8080") + "/clientip", curlConnectionTimeoutCode, true
				}),
			ginkgo.Entry("UDN pod to local node should not work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					clientPod := podsNetA[0]
					node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), clientPod.Spec.NodeName, metav1.GetOptions{})
					framework.ExpectNoError(err)
					nodeIP := node.Status.Addresses[ipFamilyIndex].Address
					// FIXME: add the host process socket to the VRF for this test to work.
					// This scenario is something that is not supported yet. So the test will continue to fail.
					// This works the same on both normal UDNs and advertised UDNs.
					// So because the process is not bound to the VRF, packet reaches the host but kernel sends a RESET. So its not code 28 but code7.
					// 10:59:55.351067 319594f193d4d_3 P   ifindex 191 0a:58:5d:5d:01:05 ethertype IPv4 (0x0800), length 80: (tos 0x0, ttl 64, id 57264,
					//    offset 0, flags [DF], proto TCP (6), length 60)
					// 93.93.1.5.36363 > 172.18.0.2.25022: Flags [S], cksum 0x0aa5 (incorrect -> 0xe0b7), seq 3879759281, win 65280,
					//    options [mss 1360,sackOK,TS val 3006752321 ecr 0,nop,wscale 7], length 0
					// 10:59:55.352404 ovn-k8s-mp87 In  ifindex 186 0a:58:5d:5d:01:01 ethertype IPv4 (0x0800), length 80: (tos 0x0, ttl 63, id 57264,
					//    offset 0, flags [DF], proto TCP (6), length 60)
					//    93.93.1.5.36363 > 172.18.0.2.25022: Flags [S], cksum 0xe0b7 (correct), seq 3879759281, win 65280,
					//    options [mss 1360,sackOK,TS val 3006752321 ecr 0,nop,wscale 7], length 0
					// 10:59:55.352461 ovn-k8s-mp87 Out ifindex 186 0a:58:5d:5d:01:02 ethertype IPv4 (0x0800), length 60: (tos 0x0, ttl 64, id 0,
					//    offset 0, flags [DF], proto TCP (6), length 40)
					//    172.18.0.2.25022 > 93.93.1.5.36363: Flags [R.], cksum 0x609d (correct), seq 0, ack 3879759282, win 0, length 0
					//    10:59:55.352927 319594f193d4d_3 Out ifindex 191 0a:58:5d:5d:01:02 ethertype IPv4 (0x0800), length 60: (tos 0x0, ttl 64, id 0,
					//    offset 0, flags [DF], proto TCP (6), length 40)
					//    172.18.0.2.25022 > 93.93.1.5.36363: Flags [R.], cksum 0x609d (correct), seq 0, ack 1, win 0, length 0
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(hostNetworkPort)) + "/hostname", "", true
				}),
			ginkgo.Entry("UDN pod to a different node should work",
				func(ipFamilyIndex int) (clientName string, clientNamespace string, dst string, expectedOutput string, expectErr bool) {
					clientPod := podsNetA[0]
					// podsNetA[0] and podsNetA[2] are on different nodes so we can pick the node of podsNetA[2] as the different node destination
					node, err := f.ClientSet.CoreV1().Nodes().Get(context.TODO(), podsNetA[2].Spec.NodeName, metav1.GetOptions{})
					framework.ExpectNoError(err)
					nodeIP := node.Status.Addresses[ipFamilyIndex].Address
					errBool := false
					out := ""
					if cudnATemplate.Spec.Network.Topology == udnv1.NetworkTopologyLayer2 {
						// FIXME: fix assymmetry in L2 UDNs
						// bad behaviour: packet is coming from other node -> entering eth0 -> bretho and here kernel drops the packet since
						// rp_filter is set to 1 in breth0 and there is an iprule that sends the packet to mpX interface so kernel sees the packet
						// having return path different from the incoming interface.
						// The SNAT to nodeIP should fix this.
						// this causes curl timeout with code 28
						errBool = true
						out = curlConnectionTimeoutCode
					}
					return clientPod.Name, clientPod.Namespace, net.JoinHostPort(nodeIP, fmt.Sprint(hostNetworkPort)) + "/hostname", out, errBool
				}),
		)

	},
	ginkgo.Entry("Layer3",
		&udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer3-network-a",
				Labels: map[string]string{"bgp-udn-layer3-network-a": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role: "Primary",
						Subnets: generateL3Subnets(udnv1.Layer3Subnet{
							CIDR:       "102.102.0.0/16",
							HostSubnet: 24,
						}, udnv1.Layer3Subnet{
							CIDR:       "2013:100:200::0/60",
							HostSubnet: 64,
						}),
					},
				},
			},
		}, &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer3-network-b",
				Labels: map[string]string{"bgp-udn-layer3-network-b": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer3,
					Layer3: &udnv1.Layer3Config{
						Role: "Primary",
						Subnets: generateL3Subnets(udnv1.Layer3Subnet{
							CIDR:       "103.103.0.0/16",
							HostSubnet: 24,
						}, udnv1.Layer3Subnet{
							CIDR:       "2014:100:200::0/60",
							HostSubnet: 64,
						}),
					},
				},
			},
		},
	),
	ginkgo.Entry("Layer2",
		&udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer2-network-a",
				Labels: map[string]string{"bgp-udn-layer2-network-a": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    "Primary",
						Subnets: generateL2Subnets("102.102.0.0/16", "2013:100:200::0/60"),
					},
				},
			},
		}, &udnv1.ClusterUserDefinedNetwork{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "bgp-udn-layer2-network-b",
				Labels: map[string]string{"bgp-udn-layer2-network-b": ""},
			},
			Spec: udnv1.ClusterUserDefinedNetworkSpec{
				Network: udnv1.NetworkSpec{
					Topology: udnv1.NetworkTopologyLayer2,
					Layer2: &udnv1.Layer2Config{
						Role:    "Primary",
						Subnets: generateL2Subnets("103.103.0.0/16", "2014:100:200::0/60"),
					},
				},
			},
		},
	),
)

func generateL3Subnets(v4, v6 udnv1.Layer3Subnet) []udnv1.Layer3Subnet {
	var subnets []udnv1.Layer3Subnet
	if isIPv4Supported() {
		subnets = append(subnets, v4)
	}
	if isIPv6Supported() {
		subnets = append(subnets, v6)
	}
	return subnets
}

func generateL2Subnets(v4, v6 string) udnv1.DualStackCIDRs {
	var subnets udnv1.DualStackCIDRs
	if isIPv4Supported() {
		subnets = append(subnets, udnv1.CIDR(v4))
	}
	if isIPv6Supported() {
		subnets = append(subnets, udnv1.CIDR(v6))
	}
	return subnets
}
