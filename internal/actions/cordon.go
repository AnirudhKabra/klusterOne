package actions

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	furyv1alpha1 "github.com/fury/fury-controller/api/v1alpha1"
)

// Cordon marks a node Unschedulable via a strategic-merge patch.
type Cordon struct {
	Client kubernetes.Interface
}

func (c *Cordon) Name() string { return string(furyv1alpha1.ActionCordon) }

func (c *Cordon) Execute(ctx context.Context, node *corev1.Node, _ furyv1alpha1.ActionSpec) error {
	if node.Spec.Unschedulable {
		return nil
	}
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	_, err := c.Client.CoreV1().Nodes().Patch(
		ctx, node.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	return err
}
