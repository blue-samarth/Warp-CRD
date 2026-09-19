package controller

import (
	"context"
	"encoding/json"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const FieldOwner = "webapp-operator"

func toApplyConfig[AC any](obj client.Object, gvk schema.GroupVersionKind) (*AC, error) {
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	out := new(AC)
	if err := json.Unmarshal(b, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *WebAppReconciler) apply(ctx context.Context, ac runtime.ApplyConfiguration) error {
	return r.Apply(ctx, ac, client.FieldOwner(FieldOwner), client.ForceOwnership)
}
