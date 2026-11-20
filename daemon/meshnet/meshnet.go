package meshnet

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/reflection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	topologyclientv1 "github.com/emigcm98/meshnet-cni/api/clientset/v1beta1"

	mpb "github.com/emigcm98/meshnet-cni/daemon/proto/meshnet/v1beta1"
)

type Config struct {
	Port     int
	GRPCOpts []grpc.ServerOption
}

type Meshnet struct {
	mpb.UnimplementedLocalServer
	mpb.UnimplementedRemoteServer
	config  Config
	kClient kubernetes.Interface
	tClient topologyclientv1.Interface
	rCfg    *rest.Config
	s       *grpc.Server
	lis     net.Listener
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

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, err
	}

	m := &Meshnet{
		config:  cfg,
		rCfg:    rCfg,
		kClient: kClient,
		tClient: tClient,
		lis:     lis,
		s:       newServerWithLogging(cfg.GRPCOpts...),
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
