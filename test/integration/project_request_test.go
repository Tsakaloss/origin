package integration

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"

	templatev1client "github.com/openshift/client-go/template/clientset/versioned"
	projectapi "github.com/openshift/origin/pkg/project/apis/project"
	"github.com/openshift/origin/pkg/project/apiserver/registry/projectrequest/delegated"
	projectclient "github.com/openshift/origin/pkg/project/generated/internalclientset"
	testutil "github.com/openshift/origin/test/util"
	testserver "github.com/openshift/origin/test/util/server"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

func TestProjectRequestError(t *testing.T) {
	const (
		ns                = "testns"
		templateNamespace = "default"
		templateName      = "project-request-template"
	)
	masterConfig, err := testserver.DefaultMasterOptions()
	if err != nil {
		t.Fatalf("error creating config: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	masterConfig.ProjectConfig.ProjectRequestTemplate = templateNamespace + "/" + templateName

	kubeConfigFile, err := testserver.StartConfiguredMaster(masterConfig)
	if err != nil {
		t.Fatalf("error starting server: %v", err)
	}
	kubeClientset, err := testutil.GetClusterAdminKubeClient(kubeConfigFile)
	if err != nil {
		t.Fatalf("error getting client: %v", err)
	}
	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(kubeConfigFile)
	if err != nil {
		t.Fatalf("error getting openshift client: %v", err)
	}

	// Create custom template
	template := delegated.DefaultTemplate()
	template.Name = templateName

	corev1Scheme := runtime.NewScheme()
	utilruntime.Must(corev1.AddToScheme(corev1Scheme))
	corev1Codec := serializer.NewCodecFactory(corev1Scheme).LegacyCodec(corev1.SchemeGroupVersion)

	// Append an object that will succeed
	template.Objects = append(template.Objects, runtime.RawExtension{
		Raw: []byte(runtime.EncodeOrDie(corev1Codec, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "configmapname"}})),
	})
	// Append a custom object that will fail validation
	template.Objects = append(template.Objects, runtime.RawExtension{
		Raw: []byte(runtime.EncodeOrDie(corev1Codec, &corev1.ConfigMap{})),
	})
	// Append another object that should never be created, since we short circuit
	template.Objects = append(template.Objects, runtime.RawExtension{
		Raw: []byte(runtime.EncodeOrDie(corev1Codec, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "configmapname2"}})),
	})

	if _, err := templatev1client.NewForConfigOrDie(clusterAdminClientConfig).TemplateV1().Templates(templateNamespace).Create(template); err != nil {
		t.Fatal(err)
	}

	// Watch the project, rolebindings, and configmaps
	nswatch, err := kubeClientset.Core().Namespaces().Watch(metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", ns).String()})
	if err != nil {
		t.Fatal(err)
	}
	roleWatch, err := kubeClientset.Rbac().RoleBindings(ns).Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cmwatch, err := kubeClientset.Core().ConfigMaps(ns).Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Create project request
	_, err = projectclient.NewForConfigOrDie(clusterAdminClientConfig).Project().ProjectRequests().Create(&projectapi.ProjectRequest{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if err == nil || err.Error() != `Internal error occurred: ConfigMap "" is invalid: metadata.name: Required value: name or generateName is required` {
		t.Fatalf("Expected internal error creating project, got %v", err)
	}

	pairCreationDeletion := func(w watch.Interface) (int, int, []watch.Event) {
		added := 0
		deleted := 0
		events := []watch.Event{}
		for {
			select {
			case e := <-w.ResultChan():
				events = append(events, e)
				switch e.Type {
				case watch.Added:
					added++
				case watch.Deleted:
					deleted++
				}
			case <-time.After(30 * time.Second):
				return added, deleted, events
			}

			if added == deleted && added > 0 {
				return added, deleted, events
			}
		}
	}

	if added, deleted, events := pairCreationDeletion(nswatch); added != deleted || added != 1 {
		for _, e := range events {
			t.Logf("%s %#v", e.Type, e.Object)
		}
		t.Errorf("expected 1 namespace to be added and deleted, got %d added / %d deleted", added, deleted)
	}
	if added, deleted, events := pairCreationDeletion(roleWatch); added != deleted || added != 4 {
		for _, e := range events {
			t.Logf("%s %#v", e.Type, e.Object)
		}
		t.Errorf("expected 4 (1 admin + 3 SA) roleBindings to be added and deleted, got %d added / %d deleted", added, deleted)
	}
	if added, deleted, events := pairCreationDeletion(cmwatch); added != deleted || added != 1 {
		for _, e := range events {
			t.Logf("%s %#v", e.Type, e.Object)
		}
		t.Errorf("expected 1 configmap to be added and deleted, got %d added / %d deleted", added, deleted)
	}

	// Verify project is deleted
	if nsObj, err := kubeClientset.Core().Namespaces().Get(ns, metav1.GetOptions{}); !kapierrors.IsNotFound(err) {
		t.Errorf("Expected namespace to be gone, got %#v, %#v", nsObj, err)
	}
}
