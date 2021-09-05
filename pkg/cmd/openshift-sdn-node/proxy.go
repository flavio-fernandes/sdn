package openshift_sdn_node

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	kubeproxyoptions "k8s.io/kubernetes/cmd/kube-proxy/app"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/pkg/proxy/userspace"

	sdnproxy "github.com/openshift/sdn/pkg/network/proxy"
	"github.com/openshift/sdn/pkg/network/proxy/unidler"
)

// readProxyConfig reads the proxy config from a file
func readProxyConfig(filename string) (*kubeproxyconfig.KubeProxyConfiguration, error) {
	o := kubeproxyoptions.NewOptions()
	o.ConfigFile = filename
	if err := o.Complete(); err != nil {
		return nil, err
	}
	return o.GetConfig(), nil
}

// initProxy sets up the proxy process.
func (sdn *openShiftSDN) initProxy() error {
	var err error
	sdn.osdnProxy, err = sdnproxy.New(
		sdn.informers.kubeClient,
		sdn.informers.kubeInformers,
		sdn.informers.osdnClient,
		sdn.informers.osdnInformers,
		sdn.proxyConfig.IPTables.MinSyncPeriod.Duration)
	return err
}

// runProxy starts the configured proxy process and closes the provided channel
// when the proxy has initialized
func (sdn *openShiftSDN) runProxy(waitChan chan<- bool) {
	if string(sdn.proxyConfig.Mode) == "disabled" {
		klog.Warningf("Built-in kube-proxy is disabled")
		sdn.startMetricsServer()
		close(waitChan)
		return
	}

	s, err := sdn.newProxyServer()
	if err != nil {
		klog.Fatalf("Unable to create proxy server: %v", err)
	}

	err = sdn.wrapProxy(s, waitChan)
	if err != nil {
		klog.Fatalf("Unable to create proxy wrapper: %v", err)
	}

	err = sdn.startProxyServer(s)
	if err != nil {
		klog.Fatalf("Unable to start proxy: %v", err)
	}

	klog.Infof("Started Kubernetes Proxy on %s", sdn.proxyConfig.BindAddress)
}

// wrapProxy wraps the created proxier with the unidling and firewalling proxies
func (sdn *openShiftSDN) wrapProxy(s *ProxyServer, waitChan chan<- bool) error {
	var err error
	var unidlingProxy sdnproxy.HybridizableProxy

	if s.enableUnidling {
		// FIXME: openshift-controller-manager assumes the LastTimestamp field in
		// the Event will be set, which is only true if we use the legacy
		// corev1.Event API rather than the new eventsv1.Event API. So we need a
		// legacy event recorder.
		unidlingBroadcaster := record.NewBroadcaster()
		unidlingBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: sdn.informers.kubeClient.CoreV1().Events("")})
		unidlingRecorder := unidlingBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kube-proxy", Host: sdn.nodeName})

		signaler := unidler.NewEventSignaler(unidlingRecorder)
		unidlingProxy, err = unidler.NewUnidlerProxier(
			userspace.NewLoadBalancerRR(),
			net.ParseIP(sdn.proxyConfig.BindAddress),
			s.IptInterface,
			s.execer,
			*utilnet.ParsePortRangeOrDie(sdn.proxyConfig.PortRange),
			sdn.proxyConfig.IPTables.SyncPeriod.Duration,
			sdn.proxyConfig.IPTables.MinSyncPeriod.Duration,
			sdn.proxyConfig.UDPIdleTimeout.Duration,
			sdn.proxyConfig.NodePortAddresses,
			signaler)
		if err != nil {
			return err
		}
	}

	sdn.osdnProxy.SetBaseProxies(s.baseProxy, unidlingProxy)
	if err := sdn.osdnProxy.Start(waitChan); err != nil {
		return err
	}

	s.Proxier = sdn.osdnProxy
	return nil
}
