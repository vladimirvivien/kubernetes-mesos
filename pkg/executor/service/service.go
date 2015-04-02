package service

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/cmd/kubelet/app"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/credentialprovider"
	_ "github.com/GoogleCloudPlatform/kubernetes/pkg/healthz"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/cadvisor"
	kconfig "github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/config"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/dockertools"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	log "github.com/golang/glog"
	"github.com/kardianos/osext"
	bindings "github.com/mesos/mesos-go/executor"
	"github.com/mesosphere/kubernetes-mesos/pkg/executor"
	"github.com/mesosphere/kubernetes-mesos/pkg/executor/config"

	"github.com/spf13/pflag"
)

const (
	MESOS_CFG_SOURCE = "mesos" // @see ConfigSourceAnnotationKey
)

type KubeletExecutorServer struct {
	*app.KubeletServer
	RunProxy               bool
	ProxyLogV              int
	ProxyExec              string
	ProxyLogfile           string
	ProxyBindall           bool
	SuicideTimeout         time.Duration
}

type hyperkubeInterface interface {
	// FindServer will find a specific server named name.
	FindServer(name string) (hyperkubeInterface, error)

	// The executable name, used for help and soft-link invocation
	Name() string

	// Flags returns a flagset for "global" flags.
	Flags() *pflag.FlagSet
}

func NewKubeletExecutorServer() *KubeletExecutorServer {
	k := &KubeletExecutorServer{
		KubeletServer:          app.NewKubeletServer(),
		RunProxy:               true,
		ProxyExec:              "./kube-proxy",
		ProxyLogfile:           "./proxy-log",
		SuicideTimeout:         config.DefaultSuicideTimeout,
	}
	if pwd, err := os.Getwd(); err != nil {
		log.Warningf("failed to determine current directory: %v", err)
	} else {
		k.RootDirectory = pwd // mesos sandbox dir
	}
	k.Address = util.IP(net.ParseIP(defaultBindingAddress()))
	k.CloudProvider = "mesos" // TODO(jdef) eventually we don't want to hardcode this
	return k
}

func NewHyperKubeletExecutorServer() *KubeletExecutorServer {
	s := NewKubeletExecutorServer()

	// cache this for later use
	binary, err := osext.Executable()
	if err != nil {
		log.Fatalf("failed to determine currently running executable: %v", err)
	}

	s.ProxyExec = binary
	return s
}

func (s *KubeletExecutorServer) addCoreFlags(fs *pflag.FlagSet) {
	s.KubeletServer.AddFlags(fs)
	fs.BoolVar(&s.RunProxy, "run_proxy", s.RunProxy, "Maintain a running kube-proxy instance as a child proc of this kubelet-executor.")
	fs.IntVar(&s.ProxyLogV, "proxy_logv", s.ProxyLogV, "Log verbosity of the child kube-proxy.")
	fs.StringVar(&s.ProxyLogfile, "proxy_logfile", s.ProxyLogfile, "Path to the kube-proxy log file.")
	fs.BoolVar(&s.ProxyBindall, "proxy_bindall", s.ProxyBindall, "When true will cause kube-proxy to bind to 0.0.0.0.")
	fs.DurationVar(&s.SuicideTimeout, "suicide_timeout", s.SuicideTimeout, "Self-terminate after this period of inactivity. Zero disables suicide watch.")
}

func (s *KubeletExecutorServer) AddStandaloneFlags(fs *pflag.FlagSet) {
	s.addCoreFlags(fs)
	fs.StringVar(&s.ProxyExec, "proxy_exec", s.ProxyExec, "Path to the kube-proxy executable.")
}

func (s *KubeletExecutorServer) AddHyperkubeFlags(fs *pflag.FlagSet) {
	s.addCoreFlags(fs)
}

// Run runs the specified KubeletExecutorServer.
func (s *KubeletExecutorServer) Run(hks hyperkubeInterface, _ []string) error {
	util.ReallyCrash = s.ReallyCrashForTesting
	rand.Seed(time.Now().UTC().UnixNano())

	if err := util.ApplyOomScoreAdj(0, s.OOMScoreAdj); err != nil {
		log.Info(err)
	}

	client, err := s.CreateAPIServerClient()
	if err != nil && len(s.APIServerList) > 0 {
		// required for k8sm since we need to send api.Binding information
		// back to the apiserver
		log.Fatalf("No API client: %v", err)
	}

	log.Infof("Using root directory: %v", s.RootDirectory)
	credentialprovider.SetPreferredDockercfgPath(s.RootDirectory)

	cadvisorInterface, err := cadvisor.New(s.CadvisorPort)
	if err != nil {
		return err
	}

	imageGCPolicy := kubelet.ImageGCPolicy{
		HighThresholdPercent: s.ImageGCHighThresholdPercent,
		LowThresholdPercent:  s.ImageGCLowThresholdPercent,
	}

	cloud := cloudprovider.InitCloudProvider(s.CloudProvider, s.CloudConfigFile)
	log.Infof("Successfully initialized cloud provider: %q from the config file: %q\n", s.CloudProvider, s.CloudConfigFile)

	hostNetworkSources, err := kubelet.GetValidatedSources(strings.Split(s.HostNetworkSources, ","))
	if err != nil {
		return err
	}
	kcfg := app.KubeletConfig{
		Address:                 s.Address,
		AllowPrivileged:         s.AllowPrivileged,
		HostNetworkSources:      hostNetworkSources,
		HostnameOverride:        s.HostnameOverride,
		RootDirectory:           s.RootDirectory,
		PodInfraContainerImage:  s.PodInfraContainerImage,
		SyncFrequency:           s.SyncFrequency,
		RegistryPullQPS:         s.RegistryPullQPS,
		RegistryBurst:           s.RegistryBurst,
		MinimumGCAge:            s.MinimumGCAge,
		MaxPerPodContainerCount: s.MaxPerPodContainerCount,
		MaxContainerCount:       s.MaxContainerCount,
		ClusterDomain:           s.ClusterDomain,
		ClusterDNS:              s.ClusterDNS,
		Port:                    s.Port,
		CadvisorInterface:       cadvisorInterface,
		EnableServer:            s.EnableServer,
		EnableDebuggingHandlers: s.EnableDebuggingHandlers,
		DockerClient:            dockertools.ConnectToDockerOrDie(s.DockerEndpoint),
		KubeClient:              client,
		MasterServiceNamespace:  s.MasterServiceNamespace,
		VolumePlugins:           app.ProbeVolumePlugins(),
		NetworkPlugins:                 app.ProbeNetworkPlugins(),
		NetworkPluginName:              s.NetworkPluginName,
		StreamingConnectionIdleTimeout: s.StreamingConnectionIdleTimeout,
		ImageGCPolicy:                  imageGCPolicy,
		Cloud:                          cloud,
	}

	finished := make(chan struct{})
	app.RunKubelet(&kcfg, app.KubeletBuilder(func(kc *app.KubeletConfig) (app.KubeletBootstrap, *kconfig.PodConfig, error) {
		return s.createAndInitKubelet(kc, hks, finished)
	}))

	// block until executor is shut down or commits suicide
	select {}
}

func defaultBindingAddress() string {
	libProcessIP := os.Getenv("LIBPROCESS_IP")
	if libProcessIP == "" {
		return "0.0.0.0"
	} else {
		return libProcessIP
	}
}

func (ks *KubeletExecutorServer) createAndInitKubelet(kc *app.KubeletConfig, hks hyperkubeInterface, finished chan struct{}) (app.KubeletBootstrap, *kconfig.PodConfig, error) {

	pc := kconfig.NewPodConfig(kconfig.PodConfigNotificationSnapshotAndUpdates, kc.Recorder)
	updates := pc.Channel(MESOS_CFG_SOURCE)

	// TODO(k8s): block until all sources have delivered at least one update to the channel, or break the sync loop
	// up into "per source" synchronizations

	kubelet, err := kubelet.NewMainKubelet(
		kc.Hostname,
		kc.DockerClient,
		kc.EtcdClient,
		kc.KubeClient,
		kc.RootDirectory,
		kc.PodInfraContainerImage,
		kc.SyncFrequency,
		float32(kc.RegistryPullQPS),
		kc.RegistryBurst,
		kc.MinimumGCAge,
		kc.MaxContainerCount,
		pc.IsSourceSeen,
		kc.ClusterDomain,
		net.IP(kc.ClusterDNS),
		kc.MasterServiceNamespace,
		kc.VolumePlugins)
	if err != nil {
		return nil, nil, err
	}

	//TODO(jdef) either configure Watch here with something useful, or else
	// get rid of it from executor.Config
	kubeletFinished := make(chan struct{})
	exec := executor.New(executor.Config{
		Kubelet:         kubelet,
		Updates:         updates,
		SourceName:      MESOS_CFG_SOURCE,
		APIClient:       kc.KubeClient,
		Docker:          kc.DockerClient,
		SuicideTimeout:  ks.SuicideTimeout,
		KubeletFinished: kubeletFinished,
	})

	k := &kubeletExecutor{
		Kubelet:                kubelet,
		finished:               finished,
		runProxy:               ks.RunProxy,
		proxyLogV:              ks.ProxyLogV,
		proxyExec:              ks.ProxyExec,
		proxyLogfile:           ks.ProxyLogfile,
		proxyBindall:           ks.ProxyBindall,
		address:                ks.Address,
		etcdServerList:         ks.EtcdServerList,
		etcdConfigFile:         ks.EtcdConfigFile,
		dockerClient:           kc.DockerClient,
		hks:                    hks,
		kubeletFinished:        kubeletFinished,
		executorDone:           exec.Done(),
	}

	dconfig := bindings.DriverConfig{
		Executor:         exec,
		HostnameOverride: ks.HostnameOverride,
		BindingAddress:   net.IP(ks.Address),
	}
	if driver, err := bindings.NewMesosExecutorDriver(dconfig); err != nil {
		log.Fatalf("failed to create executor driver: %v", err)
	} else {
		k.driver = driver
	}

	log.V(2).Infof("Initialize executor driver...")

	k.BirthCry()
	exec.Init(k.driver)

	k.StartGarbageCollection()

	return k, pc, nil
}

// kubelet decorator
type kubeletExecutor struct {
	*kubelet.Kubelet
	initialize             sync.Once
	driver                 bindings.ExecutorDriver
	finished               chan struct{} // closed once driver.Run() completes
	runProxy               bool
	proxyLogV              int
	proxyExec              string
	proxyLogfile           string
	proxyBindall           bool
	address                util.IP
	etcdServerList         util.StringList
	etcdConfigFile         string
	dockerClient           dockertools.DockerInterface
	hks                    hyperkubeInterface
	kubeletFinished        chan struct{}   // closed once kubelet.Run() returns
	executorDone           <-chan struct{} // from KubeletExecutor.Done()
}

func (kl *kubeletExecutor) ListenAndServe(address net.IP, port uint, enableDebuggingHandlers bool) {
	// this func could be called many times, depending how often the HTTP server crashes,
	// so only execute certain initialization procs once
	kl.initialize.Do(func() {
		if kl.runProxy {
			go util.Forever(kl.runProxyService, 5*time.Second)
		}
		go func() {
			defer close(kl.finished)
			if _, err := kl.driver.Run(); err != nil {
				log.Fatalf("failed to start executor driver: %v", err)
			}
			log.Info("executor Run completed")
		}()

		// TODO(who?) Recover running containers from check pointed pod list.
		// @see reconcileTasks
	})
	log.Infof("Starting kubelet server...")
	kubelet.ListenAndServeKubeletServer(kl, address, port, enableDebuggingHandlers)
}

// this function blocks as long as the proxy service is running; intended to be
// executed asynchronously.
func (kl *kubeletExecutor) runProxyService() {

	log.Infof("Starting proxy process...")

	const KM_PROXY = "proxy" //TODO(jdef) constant should be shared with km package
	args := []string{}

	if _, err := kl.hks.FindServer(KM_PROXY); err == nil {
		args = append(args, KM_PROXY)
		log.V(1).Infof("attempting to using km proxy service")
	} else if _, err := os.Stat(kl.proxyExec); os.IsNotExist(err) {
		log.Errorf("failed to locate proxy executable at '%v' and km not present: %v", kl.proxyExec, err)
		return
	}

	bindAddress := "0.0.0.0"
	if !kl.proxyBindall {
		bindAddress = kl.address.String()
	}
	args = append(args,
		fmt.Sprintf("--bind_address=%s", bindAddress),
		fmt.Sprintf("--v=%d", kl.proxyLogV),
		"--logtostderr=true",
	)
	if len(kl.etcdServerList) > 0 {
		etcdServerArguments := strings.Join(kl.etcdServerList, ",")
		args = append(args, "--etcd_servers="+etcdServerArguments)
	} else if kl.etcdConfigFile != "" {
		args = append(args, "--etcd_config="+kl.etcdConfigFile)
	}

	log.Infof("Spawning process executable %s with args '%+v'", kl.proxyExec, args)

	cmd := exec.Command(kl.proxyExec, args...)
	if _, err := cmd.StdoutPipe(); err != nil {
		log.Fatal(err)
	}

	proxylogs, err := cmd.StderrPipe()
	if err != nil {
		log.Fatal(err)
	}

	//TODO(jdef) append instead of truncate? what if the disk is full?
	logfile, err := os.Create(kl.proxyLogfile)
	if err != nil {
		log.Fatal(err)
	}
	defer logfile.Close()

	go func() {
		defer func() {
			log.Infof("killing proxy process..")
			if err = cmd.Process.Kill(); err != nil {
				log.Errorf("failed to kill proxy process: %v", err)
			}
		}()

		writer := bufio.NewWriter(logfile)
		defer writer.Flush()

		written, err := io.Copy(writer, proxylogs)
		if err != nil {
			log.Errorf("error writing data to proxy log: %v", err)
		}

		log.Infof("wrote %d bytes to proxy log", written)
	}()

	// if the proxy fails to start then we exit the executor, otherwise
	// wait for the proxy process to end (and release resources after).
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	} else if err := cmd.Wait(); err != nil {
		log.Error(err)
	}
}

// runs the main kubelet loop, closing the kubeletFinished chan when the loop exits.
// never returns.
func (kl *kubeletExecutor) Run(updates <-chan kubelet.PodUpdate) {
	defer func() {
		close(kl.kubeletFinished)
		util.HandleCrash()
		log.Infoln("kubelet run terminated") //TODO(jdef) turn down verbosity
		// important: never return! this is in our contract
		select {}
	}()

	// push updates through a closable pipe. when the executor indicates shutdown
	// via Done() we want to stop the Kubelet from processing updates.
	pipe := make(chan kubelet.PodUpdate)
	go func() {
		// closing pipe will cause our patched kubelet's syncLoop() to exit
		defer close(pipe)
	pipeLoop:
		for {
			select {
			case <-kl.executorDone:
				break pipeLoop
			default:
				select {
				case u := <-updates:
					select {
					case pipe <- u: // noop
					case <-kl.executorDone:
						break pipeLoop
					}
				case <-kl.executorDone:
					break pipeLoop
				}
			}
		}
	}()

	// we expect that Run() will complete after the pipe is closed and the
	// kubelet's syncLoop() has finished processing its backlog, which hopefully
	// will not take very long. Peeking into the future (current k8s master) it
	// seems that the backlog has grown from 1 to 50 -- this may negatively impact
	// us going forward, time will tell.
	util.Until(func() { kl.Kubelet.Run(pipe) }, 0, kl.executorDone)

	err := kl.SyncPods([]api.Pod{}) //TODO(jdef) revisit this if/when executor failover lands
	if err != nil {
		log.Errorf("failed to cleanly remove all pods and associated state: %v", err)
	}
}
