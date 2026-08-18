package util

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	APIServerReadinessLabel = "cluster-api-maas/api-server-ready"
)

// HasNamespaceLabel checks if a namespace has a specific label with the expected value
func HasNamespaceLabel(ctx context.Context, client client.Client, namespaceName, labelKey, expectedValue string) (bool, error) {
	namespace := &corev1.Namespace{}
	namespacedName := types.NamespacedName{Name: namespaceName}

	if err := client.Get(ctx, namespacedName, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if labelValue, exists := namespace.Labels[labelKey]; exists && labelValue == expectedValue {
		return true, nil
	}

	return false, nil
}
