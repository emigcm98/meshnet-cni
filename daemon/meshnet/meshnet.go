package meshnet

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/reflection"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	topologyclientv1 "github.com/emigcm98/meshnet-cni/api/clientset/v1beta1"
	"github.com/emigcm98/meshnet-cni/utils/wireutil"

	mpb "github.com/emigcm98/meshnet-cni/daemon/proto/meshnet/v1beta1"
)

type Config struct {
	Port     int
	GRPCOpts []grpc.ServerOption
}

type Meshnet struct {
	mpb.UnimplementedLocalServer
	mpb.UnimplementedRemoteServer
	config    Config
	kClient   kubernetes.Interface
	tClient   topologyclientv1.Interface
	dynClient dynamic.Interface
	rCfg      *rest.Config
	s         *grpc.Server
	lis       net.Listener
	// nodeIP is the address the plugin records as src_ip for the pods of this node, and nodeIntf
	// the interface their vxlans start from.
	nodeIP         string
	nodeIntf       string
	topoCache      *TopologyCache
	reconcileQueue *ReconcileQueue
}

var mnetdLogger = log.WithFields(log.Fields{"daemon": "meshnetd"})

func InitLogger() {
	mnetdLogger = log.WithFields(log.Fields{"daemon": "meshnetd"})
}

func restConfig() (*rest.Config, error) {
	mnetdLogger.Infof("Trying in-cluster configuration")
	rCfg, err := rest.InClusterConfig()
	if err != nil {
		kubecfg := filepath.Join(".kube", "config")
		if home := homedir.HomeDir(); home != "" {
			kubecfg = filepath.Join(home, kubecfg)
		}
		mnetdLogger.Infof("Falling back to kubeconfig: %q", kubecfg)
		rCfg, err = clientcmd.BuildConfigFromFlags("", kubecfg)
		if err != nil {
			mnetdLogger.Infof("error in Falling back to kubeconfig: %v", err)
			return nil, err
		}
	}
	// A full pass reads every Topology and writes the status of the ones it touches, which the
	// client's default rate limit of 5 requests per second turns into minutes on a large lab.
	if rCfg.QPS < 100 {
		rCfg.QPS = 100
	}
	if rCfg.Burst < 200 {
		rCfg.Burst = 200
	}
	return rCfg, nil
}

func New(cfg Config) (*Meshnet, error) {
	rCfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	kClient, err := kubernetes.NewForConfig(rCfg)
	if err != nil {
		return nil, err
	}
	tClient, err := topologyclientv1.NewForConfig(rCfg)
	if err != nil {
		return nil, err
	}
	dynClient, err := dynamic.NewForConfig(rCfg)
	if err != nil {
		return nil, err
	}

	nodeIP, nodeIntf, err := wireutil.VxlanSource(os.Getenv("HOST_IP"), os.Getenv("HOST_INTF"))
	if err != nil {
		// The vxlans fall back to the interface of the default route, and only the pods whose
		// src_ip the plugin fills with HOST_IP are recognised as local.
		mnetdLogger.Warnf("could not resolve the VXLAN source interface: %v", err)
		nodeIP = os.Getenv("HOST_IP")
	}
	mnetdLogger.Infof("Node address %s, VXLAN source interface %q", nodeIP, nodeIntf)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, err
	}

	m := &Meshnet{
		config:         cfg,
		rCfg:           rCfg,
		kClient:        kClient,
		tClient:        tClient,
		dynClient:      dynClient,
		lis:            lis,
		s:              newServerWithLogging(cfg.GRPCOpts...),
		nodeIP:         nodeIP,
		nodeIntf:       nodeIntf,
		topoCache:      NewTopologyCache(),
		reconcileQueue: NewReconcileQueue(50 * time.Millisecond),
	}
	mpb.RegisterLocalServer(m.s, m)
	mpb.RegisterRemoteServer(m.s, m)
	reflection.Register(m.s)

	return m, nil
}

func (m *Meshnet) Serve() error {
	mnetdLogger.Infof("GRPC server has started on port: %d", m.config.Port)
	return m.s.Serve(m.lis)
}

func (m *Meshnet) Stop() {
	m.s.Stop()
}

// interceptorLogger adapts a logrus logger to the v2 middleware, which no
// longer ships a logrus binding of its own.
func interceptorLogger(l log.FieldLogger) logging.Logger {
	return logging.LoggerFunc(func(_ context.Context, lvl logging.Level, msg string, fields ...any) {
		f := make(map[string]any, len(fields)/2)
		it := logging.Fields(fields).Iterator()
		for it.Next() {
			k, v := it.At()
			f[k] = v
		}
		entry := l.WithFields(f)
		switch lvl {
		case logging.LevelDebug:
			entry.Debug(msg)
		case logging.LevelWarn:
			entry.Warn(msg)
		case logging.LevelError:
			entry.Error(msg)
		default:
			entry.Info(msg)
		}
	})
}

func replaceGrpcLogger(logger *log.Entry) {
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(
		logger.WriterLevel(log.InfoLevel),
		logger.WriterLevel(log.WarnLevel),
		logger.WriterLevel(log.ErrorLevel),
	))
}

func newServerWithLogging(opts ...grpc.ServerOption) *grpc.Server {
	lEntry := log.NewEntry(log.StandardLogger())
	replaceGrpcLogger(lEntry)
	opts = append(opts,
		grpc.ChainUnaryInterceptor(logging.UnaryServerInterceptor(interceptorLogger(lEntry))),
		grpc.ChainStreamInterceptor(logging.StreamServerInterceptor(interceptorLogger(lEntry))),
	)
	return grpc.NewServer(opts...)
}
