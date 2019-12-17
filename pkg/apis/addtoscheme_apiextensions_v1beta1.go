package apis

import "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"

func init() {
	// Register the apiextensions types to the scheme
	AddToSchemes = append(AddToSchemes, v1beta1.SchemeBuilder.AddToScheme)
}
