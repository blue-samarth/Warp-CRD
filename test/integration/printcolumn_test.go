package integration_test

import (
	"encoding/json"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/blue-samarth/Warp-CRD/api/v1alpha1"
)

// Asks the apiserver for the same server-side rendered table kubectl prints,
// which is the only way to tell whether a printer column actually evaluates.
func webappTable(t *testing.T, ns string) *metav1.Table {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cfg := rest.CopyConfig(testCfg)
	cfg.GroupVersion = &schema.GroupVersion{Group: "webapps.example.com", Version: "v1alpha1"}
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(s).WithoutConversion()

	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := rc.Get().
		Namespace(ns).Resource("webapps").
		SetHeader("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io").
		DoRaw(testCtx)
	if err != nil {
		t.Fatalf("table request: %v", err)
	}
	tbl := &metav1.Table{}
	if err := json.Unmarshal(raw, tbl); err != nil {
		t.Fatalf("decode table: %v\n%s", err, raw)
	}
	return tbl
}

func TestPrinterColumn_ReadyRendersFromConditionFilter(t *testing.T) {
	ns := newNamespace(t)
	key := createApp(t, baseWebApp(ns, "printcol"))

	setDeploymentStatus(t, key, appsv1.DeploymentStatus{
		Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "MinimumReplicasAvailable"},
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"},
		},
	})

	eventually(t, func() error {
		got := &v1alpha1.WebApp{}
		if err := k8sClient.Get(testCtx, key, got); err != nil {
			return err
		}
		c := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionReady)
		if c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("Ready = %v", c)
		}
		return nil
	})

	tbl := webappTable(t, ns)

	idx := -1
	for i, c := range tbl.ColumnDefinitions {
		if c.Name == "Ready" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no Ready column in table: %+v", tbl.ColumnDefinitions)
	}
	if len(tbl.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(tbl.Rows))
	}

	cell := tbl.Rows[0].Cells[idx]
	t.Logf("Ready cell = %#v (all cells: %#v)", cell, tbl.Rows[0].Cells)
	if s, ok := cell.(string); !ok || s != "True" {
		t.Fatalf("filter-expression printer column did not evaluate: got %#v, want \"True\"", cell)
	}
}
