package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	argo_clientset "github.com/argoproj/argo-cd/pkg/client/clientset/versioned"

	// argo_cluster "github.com/crossplane-contrib/provider-argoc/cluster/v1alpha1"

	"github.com/jamiealquiza/envy"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

type ArgoCrossplaneConfig struct {
	BearerToken     string          `json:"bearerToken"`
	TLSClientConfig TLSClientConfig `json:"tlsClientConfig"`
}

type TLSClientConfig struct {
	Insecure bool   `json:"insecure"`
	CaData   string `json:"caData"`
	CertData string `json:"certData"`
	KeyData  string `json:"keyData"`
}

func main() {
	fmt.Println("Starting main...")
	envy.Parse("ARGOCROSS")
	flag.Parse()

	// connect to Kubernetes API (optional)
	// user can set KUBECONFIG via environment variable to point to a specific kubeconfig file
	kubeconfigEnv := os.Getenv("KUBECONFIG")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigEnv)
	if err != nil {
		fmt.Println("Error building kubeconfigEnv:", err.Error())
		panic(err.Error())
	}

	// set api clients up
	// kubernetes core api
	clientsetCore, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Println("Error clientsetCore:", err.Error())
		panic(err.Error())
	}

	// argo crd api
	clientsetArgo, err := argo_clientset.NewForConfig(config)
	fmt.Println("clientsetArgo: ", clientsetArgo)
	// fmt.Println("clientsetArgo*: ", *clientsetArgo)
	if err != nil {
		fmt.Println("Error clientsetArgo:", err.Error())
		panic(err.Error())
	}

	// listen for new secrets
	fmt.Println("namespace focus: ", namespace_credentials())
	factory := kubeinformers.NewSharedInformerFactoryWithOptions(clientsetCore, 0, kubeinformers.WithNamespace(namespace_credentials()))
	informer := factory.Core().V1().Secrets().Informer()
	stopper := make(chan struct{})
	defer close(stopper)

	// ##########

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	myKubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	myconfig, err := myKubeconfig.ClientConfig()
	if err != nil {
		panic(err.Error())
	}
	clientset := kubernetes.NewForConfigOrDie(myconfig)
	secretList, err := clientset.CoreV1().Secrets("crossplane-system").List(metav1.ListOptions{})

	var bearerToken string

	if err != nil {
		panic(err.Error())
	}
	for _, secret := range secretList.Items {
		if len(secret.Data["authToken"]) != 0 {
			var authToken string = string(secret.Data["authToken"])
			bearerToken = authToken
		} else {
			// fmt.Println("Skipped secret: ", secret.Name)
		}
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(new interface{}) {
			// get the secret
			var cpSecret = new.(*v1.Secret).DeepCopy()
			if len(cpSecret.Data["kubeconfig"]) == 0 {
				return
			}
			fmt.Println("Processing secret containing a kubeconfig value: ", cpSecret.GetName())
			// prepare argo config
			argoCrossplaneConfig := ArgoCrossplaneConfig{}
			// var serverKubeconfig string

			// extract data from kubeconfig containing secret
			var cpData = *&cpSecret.Data
			var clusterIP string
			var kubeConfig KubeConfig
			for k, v := range cpData {
				// fmt.Println("cpData k:", k, "v:", v)
				switch k {
				case "kubeconfig":
					err := yaml.Unmarshal(v, &kubeConfig)
					if err != nil {
						fmt.Println("not nil error")
						fmt.Println(err)
					}

					clusterIP = kubeConfig.Clusters[0].Cluster.Server

					var caData string = kubeConfig.Clusters[0].Cluster.CertificateAuthorityData
					var certData string = kubeConfig.Users[0].User.ClientCertificateData
					var keyData string = kubeConfig.Users[0].User.ClientKeyData

					fmt.Println("current-context in secret "+cpSecret.GetName()+": ", kubeConfig.CurrentContext)

					argoCrossplaneConfig.BearerToken = bearerToken
					argoCrossplaneConfig.TLSClientConfig.CaData = caData
					argoCrossplaneConfig.TLSClientConfig.Insecure = false
					argoCrossplaneConfig.TLSClientConfig.CertData = certData
					argoCrossplaneConfig.TLSClientConfig.KeyData = keyData

				}
			}
			argoCrossplaneConfigJSON, err := json.Marshal(argoCrossplaneConfig)
			if err != nil {
				fmt.Println("err argoCrossplaneConfigJSON")
				fmt.Println(err)
				return
			}

			var argoClusterName string = kubeConfig.CurrentContext

			// write kubernetes secret to argocd namespace
			// (so that argocd picks it up as a cluster)
			secret := v1.Secret{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Secret",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      cpSecret.GetName(),
					Namespace: "argocd",
					Annotations: map[string]string{
						"managed-by": "argocd.argoproj.io",
					},
					Labels: map[string]string{
						"argocd.argoproj.io/secret-type": "cluster",
					},
				},
				Data: map[string][]byte{
					"config": []byte(argoCrossplaneConfigJSON),
					"name":   []byte(argoClusterName),
					"server": []byte(clusterIP),
				},
				Type: "Opaque",
			}

			secretOut, err := clientsetCore.CoreV1().Secrets("argocd").Create(&secret)
			if err != nil {
				fmt.Println("err secretOut: ", err)
			} else {
				fmt.Println("Successfully created cluster credentials: ", secretOut.GetName())
			}

			// argoCluster := argo_cluster.Cluster()
			// fmt.Println()
			// argoCluster.Cluster{
			// 	Server: clusterIP,
			// 	Name:   argoClusterName, // cpSecret.GetName(),
			// 	Config: argo_v1alpha1.ClusterConfig{
			// 		BearerToken: bearerToken,
			// 	},
			// }
			// fmt.Println("argoCluster: ", argoCluster)
			// argo_cluster.Add(argoCluster)
			// argoClusterOut, err := clientsetArgo.ArgoprojV1alpha1() // . ("argocd").Create(&argoCluster)

			// if err != nil {
			// 	fmt.Println("err argoProjectOut")
			// 	fmt.Println(err)

			// } else {
			// 	fmt.Println("Added project", argoClusterOut.GetName())
			// }

			// // initial argo project
			// argoProject := argo_v1alpha1.AppProject{
			// 	TypeMeta: metav1.TypeMeta{
			// 		Kind:       "AppProject",
			// 		APIVersion: "argoproj.io/v1alpha1",
			// 	},
			// 	ObjectMeta: metav1.ObjectMeta{
			// 		Name: namespace(), // + "-" + argoCrossplaneConfig.AwsAuthConfig.ClusterName,
			// 	},
			// 	Spec: argo_v1alpha1.AppProjectSpec{
			// 		Description: argoClusterName + "Civo cluster owned by " + namespace(),
			// 		Destinations: []argo_v1alpha1.ApplicationDestination{
			// 			argo_v1alpha1.ApplicationDestination{
			// 				Namespace: "istio-system",
			// 				Server:    clusterIP,
			// 			},
			// 			argo_v1alpha1.ApplicationDestination{
			// 				Namespace: "istio-operator",
			// 				Server:    clusterIP,
			// 			},
			// 		},
			// 		ClusterResourceWhitelist: []metav1.GroupKind{
			// 			metav1.GroupKind{
			// 				Group: "*",
			// 				Kind:  "*",
			// 			},
			// 		},
			// 		SourceRepos: []string{"https://github.com/exocode/gitops"},
			// 		// OrphanedResources: &argo_v1alpha1.OrphanedResourcesMonitorSettings{},
			// 	},
			// }
			// argoProjectOut, err := clientsetArgo.ArgoprojV1alpha1().AppProjects("argocd").Create(&argoProject)
			// if err != nil {
			// 	fmt.Println("err argoProjectOut")
			// 	fmt.Println(err)

			// } else {
			// 	fmt.Println("Added project", argoProjectOut.GetName())
			// }

			// // initial argo project
			// argoProject := argo_v1alpha1.AppProject{
			// 	TypeMeta: metav1.TypeMeta{
			// 		Kind:       "AppProject",
			// 		APIVersion: "argoproj.io/v1alpha1",
			// 	},
			// 	ObjectMeta: metav1.ObjectMeta{
			// 		Name: namespace(), // + "-" + argoCrossplaneConfig.AwsAuthConfig.ClusterName,
			// 	},
			// 	Spec: argo_v1alpha1.AppProjectSpec{
			// 		Description: argoClusterName + "Civo cluster owned by " + namespace(),
			// 		Destinations: []argo_v1alpha1.ApplicationDestination{
			// 			argo_v1alpha1.ApplicationDestination{
			// 				Namespace: "istio-system",
			// 				Server:    clusterIP,
			// 			},
			// 			argo_v1alpha1.ApplicationDestination{
			// 				Namespace: "istio-operator",
			// 				Server:    clusterIP,
			// 			},
			// 		},
			// 		ClusterResourceWhitelist: []metav1.GroupKind{
			// 			metav1.GroupKind{
			// 				Group: "*",
			// 				Kind:  "*",
			// 			},
			// 		},
			// 		SourceRepos: []string{"https://github.com/exocode/gitops"},
			// 		// OrphanedResources: &argo_v1alpha1.OrphanedResourcesMonitorSettings{},
			// 	},
			// }
			// argoProjectOut, err := clientsetArgo.ArgoprojV1alpha1().AppProjects("argocd").Create(&argoProject)
			// if err != nil {
			// 	fmt.Println("err argoProjectOut")
			// 	fmt.Println(err)

			// } else {
			// 	fmt.Println("Added project", argoProjectOut.GetName())
			// }

			// // intial argo application
			// argoApplication := argo_v1alpha1.Application{
			// 	TypeMeta: metav1.TypeMeta{
			// 		// Kind:       argo_v1alpha1.ApplicationSchemaGroupVersionKind.String(),
			// 		// APIVersion: argo_v1alpha1.AppProjectSchemaGroupVersionKind.GroupVersion().Identifier(),
			// 		Kind:       "Application",
			// 		APIVersion: "argoproj.io/v1alpha1",
			// 	},
			// 	ObjectMeta: metav1.ObjectMeta{
			// 		Name: "infra-" + namespace() + "-" + argoClusterName,
			// 		// Finalizers: []string{"resources-finalizer.argocd.argoproj.io"},
			// 	},
			// 	Spec: argo_v1alpha1.ApplicationSpec{
			// 		Project: namespace() + "-" + argoClusterName,
			// 		Destination: argo_v1alpha1.ApplicationDestination{
			// 			Namespace: "staging",
			// 			Server:    clusterIP,
			// 		},
			// 		// Source: argo_v1alpha1.ApplicationSource{
			// 		// 	RepoURL:        "https://github.com/exocode/gitops",
			// 		// 	Path:           "user-infra",
			// 		// 	TargetRevision: "HEAD",
			// 		// },
			// 		Source: argo_v1alpha1.ApplicationSource{
			// 			RepoURL:        "https://github.com/exocode/gitops",
			// 			Path:           "",
			// 			TargetRevision: "HEAD",
			// 		},
			// 		SyncPolicy: &argo_v1alpha1.SyncPolicy{
			// 			Automated: &argo_v1alpha1.SyncPolicyAutomated{
			// 				Prune:    true,
			// 				SelfHeal: true,
			// 			},
			// 		},
			// 	},
			// }
			// argoApplicationOut, err := clientsetArgo.ArgoprojV1alpha1().Applications("argocd").Create(&argoApplication)
			// if err != nil {
			// 	fmt.Println("err argoApplicationOut")
			// 	fmt.Println(err)
			// } else {
			// 	fmt.Println("Added application", argoApplicationOut.GetName())
			// }
			// }
		},
		// TODO: Implement update function
		UpdateFunc: func(old interface{}, new interface{}) {
			fmt.Println("UpdateFunc running...")

			var oldSecret = old.(*v1.Secret).DeepCopy()
			fmt.Println("UpdateFunc oldSecret: ", oldSecret)
			var secret = new.(*v1.Secret).DeepCopy()
			fmt.Println("UpdateFunc secret: ", secret)
		},
	})

	informer.Run(stopper)
}

// get current namespace
func namespace() string {
	// This way assumes you've set the POD_NAMESPACE environment variable using the downward API.
	// This check has to be done first for backwards compatibility with the way InClusterConfig was originally set up
	if ns, ok := os.LookupEnv("CREDENTIAL_NAMESPACE"); ok {
		return ns
	}

	// Fall back to the namespace associated with the service account token, if available
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		fmt.Println("data", data)
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}

	return "default"
}

// get env namespace where to find kubeconfig
func namespace_credentials() string {
	// This way assumes you've set the CREDENTIAL_NAMESPACE environment variable using the downward API.
	// This check has to be done first for backwards compatibility with the way InClusterConfig was originally set up
	if ns, ok := os.LookupEnv("CREDENTIAL_NAMESPACE"); ok {
		return ns
	}

	// Fall back to the namespace associated with the service account token, if available
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		fmt.Println("data", data)
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}

	return "default"
}

// KubeConfig holds data from crossplane secret field "kubeconfig"
type KubeConfig struct {
	APIVersion string `json:"apiVersion"`
	Clusters   []struct {
		Cluster struct {
			CertificateAuthorityData string `json:"certificate-authority-data"`
			Server                   string `json:"server"`
		} `json:"cluster"`
		Name string `json:"name"`
	} `json:"clusters"`
	Contexts []struct {
		Context struct {
			Cluster string `json:"cluster"`
			User    string `json:"user"`
		} `json:"context"`
		Name string `json:"name"`
	} `json:"contexts"`
	CurrentContext string `json:"current-context"`
	Kind           string `json:"kind"`
	Preferences    struct {
	} `json:"preferences"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			Token                 string `json:"token"`
			ClientCertificateData string `json:"client-certificate-data"`
			ClientKeyData         string `json:"client-key-data"`
			ServerName            string `json:"server-name"`
		} `json:"user"`
	} `json:"users"`
}

type ArgoProj struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		ClusterResourceWhitelist []struct {
			Group string `json:"group"`
			Kind  string `json:"kind"`
		} `json:"clusterResourceWhitelist"`
		Description  string `json:"description"`
		Destinations []struct {
			Namespace string `json:"namespace"`
			Server    string `json:"server"`
		} `json:"destinations"`
		SourceRepos []string `json:"sourceRepos"`
	} `json:"spec"`
}
