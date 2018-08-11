/*
Copyright 2018 The Jetstack cert-manager contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package acme

import (
	"fmt"

	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/jetstack/cert-manager/pkg/acme"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	cmlisters "github.com/jetstack/cert-manager/pkg/client/listers/certmanager/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/controller"
	"github.com/jetstack/cert-manager/pkg/issuer"
)

// Acme is an issuer for an ACME server. It can be used to register and obtain
// certificates from any ACME server. It supports DNS01 and HTTP01 challenge
// mechanisms.
type Acme struct {
	*controller.Context
	issuer v1alpha1.GenericIssuer
	helper acme.Interface

	secretsLister corelisters.SecretLister
	orderLister   cmlisters.OrderLister
}

// New returns a new ACME issuer interface for the given issuer.
func New(ctx *controller.Context, issuer v1alpha1.GenericIssuer) (issuer.Interface, error) {
	if issuer.GetSpec().ACME == nil {
		return nil, fmt.Errorf("acme config may not be empty")
	}

	// TODO: invent a way to ensure WaitForCacheSync is called for all listers
	// we are interested in

	secretsLister := ctx.KubeSharedInformerFactory.Core().V1().Secrets().Lister()
	orderLister := ctx.SharedInformerFactory.Certmanager().V1alpha1().Orders().Lister()

	a := &Acme{
		Context: ctx,
		helper:  acme.NewHelper(secretsLister, ctx.ClusterResourceNamespace),
		issuer:  issuer,

		secretsLister: secretsLister,
		orderLister:   orderLister,
	}

	return a, nil
}

// Register this Issuer with the issuer factory
func init() {
	controller.RegisterIssuer(controller.IssuerACME, New)
}
