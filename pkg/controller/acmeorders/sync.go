package acmeorders

import (
	"context"
	"fmt"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/golang/glog"
	"github.com/jetstack/cert-manager/pkg/acme"
	acmecl "github.com/jetstack/cert-manager/pkg/acme/client"
	cmapi "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	acmeapi "github.com/jetstack/cert-manager/third_party/crypto/acme"
)

var (
	orderGvk = cmapi.SchemeGroupVersion.WithKind("Order")
)

// Sync will process this ACME Order.
// It is the core control function for ACME Orders, and handles:
// - creating orders
// - deciding/validated configured challenge mechanisms
// - create a Challenge resource in order to fulfill required validations
// - waiting for Challenge resources to enter the 'ready' state
func (c *Controller) Sync(ctx context.Context, o *cmapi.Order) (err error) {
	oldOrder := o
	o = o.DeepCopy()

	defer func() {
		// TODO: replace with more efficient comparison
		if reflect.DeepEqual(oldOrder.Status, o.Status) {
			return
		}
		_, updateErr := c.CMClient.CertmanagerV1alpha1().Orders(o.Namespace).Update(o)
		if err != nil {
			err = utilerrors.NewAggregate([]error{err, updateErr})
		}
	}()

	acmeHelper := &acme.Helper{
		SecretLister:             c.secretLister,
		ClusterResourceNamespace: c.Context.ClusterResourceNamespace,
	}

	genericIssuer, err := c.helper.GetGenericIssuer(o.Spec.IssuerRef, o.Namespace)
	if err != nil {
		return fmt.Errorf("error reading (cluster)issuer %q: %v", o.Spec.IssuerRef.Name, err)
	}

	cl, err := acmeHelper.ClientForIssuer(genericIssuer)
	if err != nil {
		return err
	}

	if o.Status.URL == "" {
		err := c.createOrder(ctx, cl, genericIssuer, o)
		// TODO: check for error types (perm or transient?)
		if err != nil {
			return err
		}
		return nil
	}

	// if an order is in a final state, we bail out early as there is nothing
	// left for us to do here.
	if acme.IsFinalState(o.Status.State) {
		return nil
	}

	switch o.Status.State {
	// if the status field is not set, we should check the Order with the ACME
	// server to try and populate it.
	// If this is not possible - what should we do? (???)
	case cmapi.Unknown:
		err := c.syncOrderStatus(ctx, cl, o)
		if err != nil {
			return err
		}
		// TODO: we should do something more intelligent than just returning an
		// error here.
		return fmt.Errorf("updated unknown order state. Retrying processing after applying back-off")

	// if the current state is 'ready', we need to generate a CSR and finalize
	// the order
	case cmapi.Ready:
		_, err := cl.FinalizeOrder(ctx, o.Status.FinalizeURL, o.Spec.CSR)
		errUpdate := c.syncOrderStatus(ctx, cl, o)
		if errUpdate != nil {
			// TODO: mark permenant failure?
			return fmt.Errorf("error syncing order status: %v", errUpdate)
		}
		if err != nil {
			return fmt.Errorf("error finalizing order: %v", err)
		}

		return nil

	// if the order is still pending or processing, we should continue to check
	// the state of all Challenge resources (or create challenge resources)
	case cmapi.Pending, cmapi.Processing:
		// continue

	// this is the catch-all base case for order states that we do not recognise
	default:
		return fmt.Errorf("unknown order state %q", o.Status.State)
	}

	// create a selector that we can use to find all existing Challenges for the order
	sel, err := challengeSelectorForOrder(o)
	if err != nil {
		return err
	}

	// get the list of exising challenges for this order
	existingChallenges, err := c.challengeLister.Challenges(o.Namespace).List(sel)
	if err != nil {
		return err
	}

	specsToCreate := make(map[int]cmapi.ChallengeSpec)
	for i, s := range o.Status.Challenges {
		create := true
		for _, ch := range existingChallenges {
			if s.DNSName == ch.Spec.DNSName {
				create = false
				break
			}
		}

		if !create {
			break
		}

		specsToCreate[i] = s
	}

	glog.Infof("Need to create %d challenges", len(specsToCreate))

	var errs []error
	for i, spec := range specsToCreate {
		ch := buildChallenge(i, o, spec)

		ch, err = c.CMClient.CertmanagerV1alpha1().Challenges(o.Namespace).Create(ch)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		existingChallenges = append(existingChallenges, ch)
	}

	err = utilerrors.NewAggregate(errs)
	if err != nil {
		return fmt.Errorf("error ensuring Challenge resources for Order: %v", err)
	}

	// TODO: revise this logic
	recheckOrderStatus := true
	anyChallengesFailed := false
	for _, ch := range existingChallenges {
		switch ch.Status.State {
		case cmapi.Pending, cmapi.Processing:
			recheckOrderStatus = false
		case cmapi.Failed, cmapi.Expired:
			anyChallengesFailed = true
		}
	}

	// if at least 1 order is not valid, AND no orders have failed, we should
	// just return early and not query the ACME API.
	if !recheckOrderStatus && !anyChallengesFailed {
		glog.Infof("Waiting for all challenges for order %q to enter 'ready' state", o.Name)
		return nil
	}

	// otherwise, sync the order state with the ACME API.
	err = c.syncOrderStatus(ctx, cl, o)
	if err != nil {
		return err
	}

	return nil
}

const (
	orderNameLabelKey = "acme.cert-manager.io/order-name"
)

func (c *Controller) createOrder(ctx context.Context, cl acmecl.Interface, issuer cmapi.GenericIssuer, o *cmapi.Order) error {
	if o.Status.URL != "" {
		return fmt.Errorf("refusing to recreate a new order for Order %q. Please create a new Order resource to initiate a new order", o.Name)
	}

	identifierSet := sets.NewString(o.Spec.DNSNames...)
	if o.Spec.CommonName != "" {
		identifierSet.Insert(o.Spec.CommonName)
	}
	// create a new order with the acme server
	orderTemplate := acmeapi.NewOrder(identifierSet.List()...)
	acmeOrder, err := cl.CreateOrder(ctx, orderTemplate)
	if err != nil {
		return fmt.Errorf("error creating new order: %v", err)
	}

	setOrderStatus(&o.Status, acmeOrder)

	chals := make([]cmapi.ChallengeSpec, len(acmeOrder.Authorizations))
	// we only set the status.challenges field when we first create the order,
	// because we only create one order per Order resource.
	for i, authzURL := range acmeOrder.Authorizations {
		authz, err := cl.GetAuthorization(ctx, authzURL)
		if err != nil {
			return err
		}

		cs, err := c.challengeSpecForAuthorization(ctx, cl, issuer, o, authz)
		if err != nil {
			return fmt.Errorf("Error constructing Challenge resource for Authorization: %v", err)
		}

		chals[i] = *cs
	}
	o.Status.Challenges = chals

	return nil
}

func (c *Controller) challengeSpecForAuthorization(ctx context.Context, cl acmecl.Interface, issuer cmapi.GenericIssuer, o *cmapi.Order, authz *acmeapi.Authorization) (*cmapi.ChallengeSpec, error) {
	cfg, err := solverConfigurationForAuthorization(o.Spec.Config, authz)
	if err != nil {
		return nil, err
	}

	acmeSpec := issuer.GetSpec().ACME
	if acmeSpec == nil {
		return nil, fmt.Errorf("issuer %q is not configured as an ACME Issuer. Cannot be used for creating ACME orders", issuer.GetObjectMeta().Name)
	}

	var challenge *acmeapi.Challenge
	for _, ch := range authz.Challenges {
		switch {
		case ch.Type == "http-01" && cfg.HTTP01 != nil && acmeSpec.HTTP01 != nil:
			challenge = ch
		case ch.Type == "dns-01" && cfg.DNS01 != nil && acmeSpec.DNS01 != nil:
			challenge = ch
		}
	}

	domain := authz.Identifier.Value
	if challenge == nil {
		return nil, fmt.Errorf("ACME server does not allow selected challenge type or no provider is configured for domain %q", domain)
	}

	key, err := keyForChallenge(cl, challenge)
	if err != nil {
		return nil, err
	}

	return &cmapi.ChallengeSpec{
		AuthzURL:  authz.URL,
		Type:      challenge.Type,
		URL:       challenge.URL,
		DNSName:   domain,
		Token:     challenge.Token,
		Key:       key,
		Config:    *cfg,
		Wildcard:  authz.Wildcard,
		IssuerRef: o.Spec.IssuerRef,
	}, nil
}

func keyForChallenge(cl acmecl.Interface, challenge *acmeapi.Challenge) (string, error) {
	var err error
	switch challenge.Type {
	case "http-01":
		return cl.HTTP01ChallengeResponse(challenge.Token)
	case "dns-01":
		return cl.DNS01ChallengeRecord(challenge.Token)
	default:
		err = fmt.Errorf("unsupported challenge type %s", challenge.Type)
	}
	return "", err
}

func solverConfigurationForAuthorization(cfgs []cmapi.DomainSolverConfig, authz *acmeapi.Authorization) (*cmapi.SolverConfig, error) {
	domainToFind := authz.Identifier.Value
	if authz.Wildcard {
		domainToFind = "*." + domainToFind
	}
	for _, d := range cfgs {
		for _, dom := range d.Domains {
			if dom != domainToFind {
				continue
			}
			return &d.SolverConfig, nil
		}
	}
	return nil, fmt.Errorf("solver configuration for domain %q not found. Ensure you have configured a challenge mechanism using the certificate.spec.acme.config field", domainToFind)
}

// syncOrderStatus will communicate with the ACME server to retrieve the current
// state of the Order. It will then update the Order's status block with the new
// state of the order.
func (c *Controller) syncOrderStatus(ctx context.Context, cl acmecl.Interface, o *cmapi.Order) error {
	if o.Status.URL == "" {
		return fmt.Errorf("order URL is blank - order has not been created yet")
	}

	acmeOrder, err := cl.GetOrder(ctx, o.Status.URL)
	if err != nil {
		// TODO: handle 404 acme responses and mark the order as failed
		return err
	}

	setOrderStatus(&o.Status, acmeOrder)

	return nil
}

func buildChallenge(i int, o *cmapi.Order, chalSpec cmapi.ChallengeSpec) *cmapi.Challenge {
	ch := &cmapi.Challenge{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-%d", o.Name, i),
			Labels:          challengeLabelsForOrder(o),
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(o, orderGvk)},
		},
		Spec: chalSpec,
	}

	return ch
}

// setOrderStatus will populate the given OrderStatus struct with the details from
// the provided ACME Order.
func setOrderStatus(o *cmapi.OrderStatus, acmeOrder *acmeapi.Order) {
	// TODO: should we validate the State returned by the ACME server here?
	cmState := cmapi.State(acmeOrder.Status)
	setOrderState(o, cmState)

	o.URL = acmeOrder.URL
	o.FinalizeURL = acmeOrder.FinalizeURL
	o.CertificateURL = acmeOrder.CertificateURL
}

func challengeLabelsForOrder(o *cmapi.Order) map[string]string {
	return map[string]string{
		orderNameLabelKey: o.Name,
	}
}

// challengeSelectorForOrder will construct a labels.Selector that can be used to
// find Challenges associated with the given Order.
func challengeSelectorForOrder(o *cmapi.Order) (labels.Selector, error) {
	lbls := challengeLabelsForOrder(o)
	var reqs []labels.Requirement
	for k, v := range lbls {
		req, err := labels.NewRequirement(k, selection.Equals, []string{v})
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, *req)
	}
	return labels.NewSelector().Add(reqs...), nil
}

// setOrderState will set the 'State' field of the given Order to 's'.
// It will set the Orders failureTime field if the state provided is classed as
// a failure state.
func setOrderState(o *cmapi.OrderStatus, s cmapi.State) {
	o.State = s
	// if the order is in a failure state, we should set the `failureTime` field
	if acme.IsFailureState(o.State) {
		t := metav1.NewTime(time.Now())
		o.FailureTime = &t
	}
}
