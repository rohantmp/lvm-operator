/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"context"
	"flag"
	"net"
	"os"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/cybozu-go/well"
	lvmv1alpha1 "github.com/red-hat-storage/lvm-operator/api/v1alpha1"
	"github.com/red-hat-storage/lvm-operator/pkg/vgmanager"
	"github.com/topolvm/topolvm/lvmd"
	"github.com/topolvm/topolvm/lvmd/command"
	lvmdProto "github.com/topolvm/topolvm/lvmd/proto"
	lvmdCMD "github.com/topolvm/topolvm/pkg/lvmd/cmd"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(lvmv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	configChannel := make(chan lvmdCMD.Config, 1)

	if err = (&vgmanager.VGReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		ConfigChannel: configChannel,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LVMCluster")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	go func() {
		err = startLVMd(configChannel)
		if err != nil {
			setupLog.Error(err, "lvmd exited with error")
			os.Exit(1)
		}
	}()

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

var logger = ctrl.Log.WithName("lvmd-wrapper")

func startLVMd(configChannel chan lvmdCMD.Config) error {
	err := well.LogConfig{}.Apply()
	if err != nil {
		logger.Error(err, "failed to apply well logconfig to lvmd")
		return err
	}

	// handle context canceled with Done(), exit process with error?

	var env *well.Environment

	for {
		config, closed := <-configChannel
		if closed {
			break
		}
		if env != nil {
			env.Cancel(nil)
			env.Wait() //nolint:golint,errcheck
		}
		env = well.NewEnvironment(context.Background())
		var err error
		go func() {
			err = runAndWait(config, env)
		}()
		if err != nil {
			logger.Error(err, "lvmd exited")
			return err
		} else {
			logger.Info("lvmd exited gracefully")
		}

	}
	return nil
}

func runAndWait(config lvmdCMD.Config, env *well.Environment) error {

	// using the same log package as topolvm for identical troubleshooting
	logger.Info("configuration file loaded",
		"device_classes", config.DeviceClasses,
		"socket_name", config.SocketName,
		"file_name", vgmanager.ConfigFilePath,
	)
	err := lvmd.ValidateDeviceClasses(config.DeviceClasses)
	if err != nil {
		return err
	}
	for _, dc := range config.DeviceClasses {
		_, err := command.FindVolumeGroup(dc.VolumeGroup)
		if err != nil {
			logger.Error(err, "Volume group not found:",
				"volume_group", dc.VolumeGroup,
			)
			return err
		}
	}

	// UNIX domain socket file should be removed before listening.
	err = os.Remove(config.SocketName)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	lis, err := net.Listen("unix", config.SocketName)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer()
	manager := lvmd.NewDeviceClassManager(config.DeviceClasses)
	vgService, notifier := lvmd.NewVGService(manager)
	lvmdProto.RegisterVGServiceServer(grpcServer, vgService)
	lvmdProto.RegisterLVServiceServer(grpcServer, lvmd.NewLVService(manager, notifier))
	well.Go(func(ctx context.Context) error {
		return grpcServer.Serve(lis)
	})
	well.Go(func(ctx context.Context) error {
		<-ctx.Done()
		grpcServer.GracefulStop()
		return nil
	})
	well.Go(func(ctx context.Context) error {
		ticker := time.NewTicker(10 * time.Minute)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return nil
			case <-ticker.C:
				notifier()
			}
		}
	})
	err = well.Wait()
	if err != nil && !well.IsSignaled(err) {
		return err
	}
	return nil
}
