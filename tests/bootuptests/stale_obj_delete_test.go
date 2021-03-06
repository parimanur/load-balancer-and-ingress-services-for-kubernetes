package bootuptests

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/cache"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/k8s"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/lib"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/tests/integrationtest"

	crdfake "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/internal/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

var KubeClient *k8sfake.Clientset
var CRDClient *crdfake.Clientset
var ctrl *k8s.AviController
var restChan chan bool
var uuidMap map[string]bool

const mockFilePath = "bootupmock"
const invalidFilePath = "invalidmock1"

var FakeAviObjects = []string{
	"cloud",
	"ipamdnsproviderprofile",
	"network",
	"pool",
	"poolgroup",
	"virtualservice",
	"vrfcontext",
	"vsdatascriptset",
	"serviceenginegroup",
	"vsvip",
}

func TestMain(m *testing.M) {
	os.Setenv("INGRESS_API", "extensionv1")
	os.Setenv("NETWORK_NAME", "net123")
	os.Setenv("CLUSTER_NAME", "cluster")
	os.Setenv("CLOUD_NAME", "CLOUD_VCENTER")
	os.Setenv("SEG_NAME", "Default-Group")
	os.Setenv("NODE_NETWORK_LIST", `[{"networkName":"net123","cidrs":["10.79.168.0/22"]}]`)
	utils.CtrlVersion = "20.1.1"
	restChan = make(chan bool)
	uuidMap = make(map[string]bool)

	KubeClient = k8sfake.NewSimpleClientset()
	CRDClient = crdfake.NewSimpleClientset()
	lib.SetCRDClientset(CRDClient)

	registeredInformers := []string{
		utils.ServiceInformer,
		utils.EndpointInformer,
		utils.IngressInformer,
		utils.SecretInformer,
		utils.NSInformer,
		utils.NodeInformer,
		utils.ConfigMapInformer,
	}
	utils.NewInformers(utils.KubeClientIntf{KubeClient}, registeredInformers)
	k8s.NewCRDInformers(CRDClient)

	mcache := cache.SharedAviObjCache()
	cloudObj := &cache.AviCloudPropertyCache{Name: "Default-Cloud", VType: "mock"}
	subdomains := []string{"avi.internal", ".com"}
	cloudObj.NSIpamDNS = subdomains
	mcache.CloudKeyCache.AviCacheAdd("Default-Cloud", cloudObj)

	integrationtest.InitializeFakeAKOAPIServer()

	integrationtest.NewAviFakeClientInstance(true)
	defer integrationtest.AviFakeClientInstance.Close()

	os.Exit(m.Run())
}

func addConfigMap() {
	aviCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "avi-system",
			Name:      "avi-k8s-config",
		},
	}
	KubeClient.CoreV1().ConfigMaps("avi-system").Create(aviCM)

}

func injectMWForObjDeletion() {
	integrationtest.AddMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var finalResponse []byte
		url := r.URL.EscapedPath()
		object := strings.Split(strings.Trim(url, "/"), "/")
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusNoContent)
			w.Write(finalResponse)
			uuid := object[2]
			utils.AviLog.Infof("uuid of the object for deletion: %s", uuid)
			if _, found := uuidMap[uuid]; found {
				delete(uuidMap, uuid)
			} else {
				utils.AviLog.Warnf("unexpcted object for deletion: %s", uuid)
				restChan <- false
			}
			// We expect all objects to be deleted in the end
			if len(uuidMap) == 0 {
				restChan <- true
			}
		} else if r.Method == "GET" {
			integrationtest.FeedMockCollectionData(w, r, mockFilePath)

		} else if strings.Contains(url, "login") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"success": "true"}`))
		}
	})
}

func injectMWForCloud() {
	integrationtest.AddMiddleware(func(w http.ResponseWriter, r *http.Request) {
		url := r.URL.EscapedPath()
		if r.Method == "GET" && strings.Contains(url, "/api/cloud/") {
			integrationtest.FeedMockCollectionData(w, r, invalidFilePath)

		} else if r.Method == "GET" {
			integrationtest.FeedMockCollectionData(w, r, mockFilePath)

		} else if strings.Contains(url, "login") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"success": "true"}`))
		}
	})
}

// Wait for true or false on rest channel to confirm object deletion
func waitAndverify(t *testing.T) {
	waitChan := make(chan int)
	go func() {
		time.Sleep(50 * time.Second)
		waitChan <- 1
	}()

	select {
	case data := <-restChan:
		if data == false {
			t.Fatalf("error in stale object deletion")
		}
		t.Logf("xxx here")
	case _ = <-waitChan:
		t.Fatalf("timed out waiting for object deletion")

	}
}

// PopulateCache populates cache and triggers deletion of unused objects.
// In this case two pool and two vsvip objects. Among these, one vsvip is
// refeered by a Virtual Service, so we need to delete 3 objects
func TestObjDeletion(t *testing.T) {
	uuidMap["pool-e3b87aff-a9d7-44eb-9935-6fd9ab81a37c"] = true
	uuidMap["pool-11a38043-e51e-4c93-8187-b390d7d81abd"] = true
	uuidMap["vsvip-a590042a-358f-4693-bfa5-cb9d0c8c1931"] = true
	//uuidMap["vsvip-82b41dd7-5b19-4007-85d4-530acea4d86b"] = true

	injectMWForObjDeletion()
	addConfigMap()
	go k8s.PopulateCache()
	// DeleteConfigMap(t)
	integrationtest.ResetMiddleware()
}

// Injecting middleware to error out cloud properties cache update failure
func TestNetworkIssueCacheValidationDuringBootup(t *testing.T) {
	injectMWForCloud()
	err := k8s.PopulateCache()
	if err == nil {
		t.Fatalf("Cache validation failed.")
	}
	integrationtest.ResetMiddleware()
}
