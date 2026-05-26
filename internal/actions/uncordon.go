package actions

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	kov1alpha1 "github.com/AnirudhKabra/klusterOne/api/v1alpha1"
)

// Uncordon reverses Cordon. It's the typical final step of a maintenance run.
type Uncordon struct {
	Client kubernetes.Interface
}

func (u *Uncordon) Name() string { return string(kov1alpha1.ActionUncordon) }

func (u *Uncordon) Execute(
	ctx context.Context,
	_ *kov1alpha1.NodeMaintenance,
	node *corev1.Node,
	_ *kov1alpha1.NodeStatus,
	_ kov1alpha1.ActionSpec,
) error {
	if !node.Spec.Unschedulable {
		return nil
	}
	patch := []byte(`{"spec":{"unschedulable":false}}`)
	_, err := u.Client.CoreV1().Nodes().Patch(
		ctx, node.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{},
	)
	return err
}
