package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

type status struct {
	code int
	msg  string
}

const (
	defaultConfigFilename      = "go-and-lead.yaml"
	defaultKubeConfig          = "~/kubeconfig"
	envPrefix                  = "GAL"
	replaceHyphenWithCamelCase = false
)

var (
	livenessStatus = status{
		code: 200,
		msg:  "Running...",
	}
	electionSataus = status{
		code: 503,
		msg:  "No leader elected",
	}
)

// Handle liveness probe
// Indicates whether the container is running.
func LivenessHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(livenessStatus.code)
	w.Write([]byte(livenessStatus.msg))
}

// Handle readiness probe
// Indicates whether the container is ready to respond to requests.
func ElectionHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(electionSataus.code)
	w.Write([]byte(electionSataus.msg))
}

func main() {
	cmd := GoAndLeadCmd()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func GoAndLeadCmd() *cobra.Command {

	// Flag variables
	var id string
	var name string
	var namespace string
	var kubeconfig string
	var svc string
	var svcSelector string
	var incluster bool
	var port int

	// Define our command
	rootCmd := &cobra.Command{
		Use:   "goandlead",
		Short: "Client-go based leader election for k8s",
		Long:  `GoAndLead is a small leader election tool for kubernetes. Via this tool it's possible to perform leader election and get the status via a readiness endpoint.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// You can bind cobra and viper in a few locations, but PersistencePreRunE on the root command works well
			return initializeConfig(cmd)
		},
		Run: func(cmd *cobra.Command, args []string) {
			/**
			 * Initialize the kubernetis client
			 */
			var config *rest.Config

			if incluster {
				var err error
				config, err = rest.InClusterConfig()
				if err != nil {
					klog.Fatal(err)
				}
			} else {
				var err error
				if kubeconfig == defaultKubeConfig {
					home, err := os.UserHomeDir()
					if err != nil {
						klog.Fatal(err)
					}
					kubeconfig = filepath.Join(home, "kubeconfig")
				}
				config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
				if err != nil {
					klog.Fatal(err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			k8s := kubernetes.NewForConfigOrDie(config)

			serviceClient := k8s.CoreV1().Services(namespace)

			// listen for interrupts or the Linux SIGTERM signal and cancel
			// our context, which the leader election code will observe and
			// step down
			ch := make(chan os.Signal, 1)
			signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-ch
				klog.Info("Received termination, signaling shutdown")
				cancel()
			}()

			// we use the Lease lock type since edits to Leases are less common
			// and fewer objects in the cluster watch "all Leases".
			lock := &resourcelock.LeaseLock{
				LeaseMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Client: k8s.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{
					Identity: id,
				},
			}

			// start the leader election code loop
			go leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
				Lock:            lock,
				ReleaseOnCancel: true,
				LeaseDuration:   60 * time.Second,
				RenewDeadline:   15 * time.Second,
				RetryPeriod:     5 * time.Second,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(context.Context) {
						// Do nothing?!
					},
					OnStoppedLeading: func() {
						// we can do cleanup here
						klog.Infof("leader lost: %s", id)
						electionSataus = status{
							code: 503,
							msg:  fmt.Sprintf("successfully acquired lease %s/%s", namespace, name),
						}
						livenessStatus = status{
							code: 503,
							msg:  fmt.Sprintf("successfully acquired lease %s/%s", namespace, name),
						}
						if svc != "" {
							// Update svc and remove leader
							klog.Infof("Updating svc ...")
							svcToUpdate, errSvcGet := serviceClient.Get(ctx, svc, metav1.GetOptions{})
							Fatal(errSvcGet)

							if svcToUpdate.Spec.Selector[svcSelector] == id {
								delete(svcToUpdate.Spec.Selector, svcSelector)
								_, errSvcApply := serviceClient.Update(ctx, svcToUpdate, metav1.UpdateOptions{})
								Fatal(errSvcApply)
								klog.Infof("Updated svc, removed svc.Spec.Selector[%s]", svcSelector)
							}
						}
						os.Exit(0)
					},
					OnNewLeader: func(identity string) {
						// we're notified when new leader elected
						if identity == id {
							// We acquired the lease
							electionSataus = status{
								code: 200,
								msg:  fmt.Sprintf("successfully acquired lease %s/%s", namespace, name),
							}
							if svc != "" {
								// Update svc to match
								klog.Infof("Updating svc ...")
								svcToUpdate, errSvcGet := serviceClient.Get(ctx, svc, metav1.GetOptions{})
								Fatal(errSvcGet)

								svcToUpdate.Spec.Selector[svcSelector] = id
								_, errSvcApply := serviceClient.Update(ctx, svcToUpdate, metav1.UpdateOptions{})
								Fatal(errSvcApply)
								klog.Infof("Updated svc.Spec.Selector[%s]=%s", svcSelector, id)
							}

							klog.Infof("successfully acquired lease %s/%s", namespace, name)
							return
						}
						// Someone else acquired the lease
						electionSataus = status{
							code: 503,
							msg:  fmt.Sprintf("new leader elected: %s", identity),
						}
						klog.Infof("new leader elected: %s", identity)
					},
				},
			})

			/**
			 * Start the webserver, that will answer to the readiness and liveness probes
			 */
			mux := http.NewServeMux()
			addr := fmt.Sprintf(":%d", port)
			mux.HandleFunc("/election", ElectionHandler)
			mux.HandleFunc("/liveness", LivenessHandler)
			log.Printf("server is listening at %s...", addr)
			http.ListenAndServe(addr, mux)

		},
	}

	// Define cobra flags, the default value has the lowest (least significant) precedence
	rootCmd.Flags().IntVarP(&port, "port", "p", 80, "The port on which the http server will run and serve the health endpoints.")
	rootCmd.Flags().StringVarP(&id, "id", "i", uuid.New().String(), "The identity id of the (this) lease lock holder.")
	rootCmd.Flags().StringVarP(&name, "name", "n", "default", "The name of the lease lock that will be used.")
	rootCmd.Flags().StringVarP(&namespace, "namespace", "s", "default", "The namespace to use for the lease lock.")
	rootCmd.Flags().StringVarP(&kubeconfig, "kubeconfig", "k", defaultKubeConfig, "The kubeconfig to use for interaction with kubernetis.")
	rootCmd.Flags().BoolVarP(&incluster, "in-cluster", "c", true, "Use serviceaccount permissions to interact with kubernetis.")
	rootCmd.Flags().StringVarP(&svc, "svc", "", "", "Update a k8s service and change the selector to match the leader id.")
	rootCmd.Flags().StringVarP(&svcSelector, "svc-selector", "", "statefulset.kubernetes.io/pod-name", "Update the k8s service and change the selector to match the leader id.")
	return rootCmd
}

func initializeConfig(cmd *cobra.Command) error {
	v := viper.New()
	v.SetConfigName(defaultConfigFilename)
	v.AddConfigPath(".")

	/**
	 * Attempt to read the config file, gracefully ignoring errors
	 * caused by a config file not being found. Return an error
	 * if we cannot parse the config file.
	 */
	if err := v.ReadInConfig(); err != nil {
		// It's okay if there isn't a config file
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return err
		}
	}

	v.SetEnvPrefix(envPrefix)

	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	/**
	 * Bind to environment variables
	 * Works great for simple config names, but needs help for names
	 * like --favorite-color which we fix in the bindFlags function
	 */
	v.AutomaticEnv()

	// Bind the current command's flags to viper
	bindFlags(cmd, v)

	return nil
}

// Bind each cobra flag to its associated viper configuration (config file and environment variable)
func bindFlags(cmd *cobra.Command, v *viper.Viper) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		// Determine the naming convention of the flags when represented in the config file
		configName := f.Name
		/**
		 * If using camelCase in the config file, replace hyphens with a camelCased string.
		 * Since viper does case-insensitive comparisons, we don't need to bother fixing the case,
		 * and only need to remove the hyphens.
		 */
		if replaceHyphenWithCamelCase {
			configName = strings.ReplaceAll(f.Name, "-", "")
		}

		// Apply the viper config value to the flag when the flag is not set and viper has a value
		if !f.Changed && v.IsSet(configName) {
			val := v.Get(configName)
			cmd.Flags().Set(f.Name, fmt.Sprintf("%v", val))
		}
	})
}

func Fatal(err error) {
	if err != nil {
		panic(errors.Wrap(err, "Error:"))
	}
}
