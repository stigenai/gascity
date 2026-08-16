package k8s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	capsuleNetworkPolicyLabel = "gc-capsule-network"
	capsuleNetworkModeLabel   = "gc-capsule-network-mode"
	capsuleInstanceAnnotation = "gascity.dev/capsule-instance-sha256"
	modelEgressLabel          = "gascity.dev/capsule-egress"
	modelEgressValue          = "model"
)

type capsuleNetworkPolicyReference struct {
	Name string
	UID  types.UID
}

func (p *Provider) ensureCapsuleNetworkPolicy(ctx context.Context, name string, cfg runtime.Config) (capsuleNetworkPolicyReference, bool, error) {
	if cfg.Capsule == nil {
		return capsuleNetworkPolicyReference{}, false, nil
	}
	if p.networkOps == nil {
		return capsuleNetworkPolicyReference{}, false, fmt.Errorf("kubernetes network policy operations are unavailable")
	}
	if err := p.preflightCapsuleNetwork(ctx, cfg); err != nil {
		return capsuleNetworkPolicyReference{}, false, err
	}

	want, err := p.buildCapsuleNetworkPolicy(name, cfg)
	if err != nil {
		return capsuleNetworkPolicyReference{}, false, err
	}
	existing, err := p.networkOps.getNetworkPolicy(ctx, want.Name)
	if err == nil {
		if err := validateCapsuleNetworkPolicy(existing, want); err != nil {
			return capsuleNetworkPolicyReference{}, false, err
		}
		return capsuleNetworkPolicyReference{Name: existing.Name, UID: existing.UID}, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return capsuleNetworkPolicyReference{}, false, fmt.Errorf("get capsule NetworkPolicy %q: %w", want.Name, err)
	}
	created, err := p.networkOps.createNetworkPolicy(ctx, want)
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := p.networkOps.getNetworkPolicy(ctx, want.Name)
		if getErr != nil {
			return capsuleNetworkPolicyReference{}, false, fmt.Errorf("reopen concurrently created capsule NetworkPolicy %q: %w", want.Name, getErr)
		}
		if validateErr := validateCapsuleNetworkPolicy(existing, want); validateErr != nil {
			return capsuleNetworkPolicyReference{}, false, validateErr
		}
		return capsuleNetworkPolicyReference{Name: existing.Name, UID: existing.UID}, false, nil
	}
	if err != nil {
		return capsuleNetworkPolicyReference{}, false, fmt.Errorf("create capsule NetworkPolicy %q: %w", want.Name, err)
	}
	if created == nil || created.UID == "" {
		return capsuleNetworkPolicyReference{}, false, fmt.Errorf("create capsule NetworkPolicy %q returned no immutable UID", want.Name)
	}
	return capsuleNetworkPolicyReference{Name: created.Name, UID: created.UID}, true, nil
}

func (p *Provider) preflightCapsuleNetwork(ctx context.Context, cfg runtime.Config) error {
	if cfg.Capsule != nil && cfg.Capsule.Network == runtime.CapsuleNetworkExternalModel {
		gateways, err := p.ops.listPods(ctx, modelEgressLabel+"="+modelEgressValue, "status.phase=Running")
		if err != nil {
			return fmt.Errorf("discover model-egress gateway: %w", err)
		}
		if len(gateways) == 0 {
			return fmt.Errorf("external-model capsule requires a running pod labeled %s=%s in namespace %q", modelEgressLabel, modelEgressValue, p.namespace)
		}
	}
	return nil
}

func (p *Provider) buildCapsuleNetworkPolicy(name string, cfg runtime.Config) (*networkingv1.NetworkPolicy, error) {
	mode := cfg.Capsule.Network
	ledgerPort, err := strconv.Atoi(p.managedServicePort)
	if err != nil || ledgerPort < 1 || ledgerPort > 65535 {
		return nil, fmt.Errorf("invalid capsule ledger port %q", p.managedServicePort)
	}
	instanceFingerprint := capsuleInstanceFingerprint(cfg)
	policyName := capsuleNetworkPolicyName(name, instanceFingerprint)
	protocolTCP := corev1.ProtocolTCP
	protocolUDP := corev1.ProtocolUDP
	dnsPort := intstr.FromInt32(53)
	ledger := intstr.FromInt(ledgerPort)
	modelPort := intstr.FromInt32(443)
	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocolUDP, Port: &dnsPort}, {Protocol: &protocolTCP, Port: &dnsPort}},
		},
		{
			To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dolt"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocolTCP, Port: &ledger}},
		},
	}
	if mode == runtime.CapsuleNetworkExternalModel {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{modelEgressLabel: modelEgressValue}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocolTCP, Port: &modelPort}},
		})
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName, Namespace: p.namespace,
			Labels: map[string]string{
				capsuleNetworkPolicyLabel: "true", capsuleNetworkModeLabel: string(mode), "gc-session": SanitizeLabel(name),
			},
			Annotations: map[string]string{capsuleInstanceAnnotation: instanceFingerprint},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"gc-session": SanitizeLabel(name), "gc-capsule": "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}, nil
}

func validateCapsuleNetworkPolicy(got, want *networkingv1.NetworkPolicy) error {
	if got == nil || got.UID == "" || got.Name != want.Name || got.Namespace != want.Namespace || !reflect.DeepEqual(got.Labels, want.Labels) || !reflect.DeepEqual(got.Annotations, want.Annotations) || !reflect.DeepEqual(got.Spec, want.Spec) {
		return fmt.Errorf("capsule NetworkPolicy %q conflicts with required isolation", want.Name)
	}
	return nil
}

func (p *Provider) deleteCapsuleNetworkPolicies(ctx context.Context, sessionLabel, instanceFingerprint string) error {
	if p.networkOps == nil {
		return nil
	}
	policies, err := p.networkOps.listNetworkPolicies(ctx, capsuleNetworkPolicyLabel+"=true,gc-session="+sessionLabel)
	if err != nil {
		return fmt.Errorf("list capsule NetworkPolicies: %w", err)
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].Name < policies[j].Name })
	var errs []error
	for i := range policies {
		policy := &policies[i]
		if instanceFingerprint != "" && policy.Annotations[capsuleInstanceAnnotation] != instanceFingerprint {
			continue
		}
		if err := p.networkOps.deleteNetworkPolicy(ctx, policy.Name, policy.UID); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			errs = append(errs, fmt.Errorf("delete capsule NetworkPolicy %q: %w", policy.Name, err))
		}
	}
	return errors.Join(errs...)
}

func capsuleNetworkPolicyName(name, fingerprint string) string {
	base := SanitizeName(name)
	suffix := fingerprint[:12]
	if len(base) > 49 {
		base = strings.TrimRight(base[:49], "-")
	}
	return base + "-np-" + suffix
}

func capsuleInstanceFingerprint(cfg runtime.Config) string {
	value := strings.TrimSpace(cfg.Env["GC_INSTANCE_TOKEN"])
	if value == "" && cfg.Capsule != nil {
		value = cfg.Capsule.Key.Digest
	}
	return capsuleTokenFingerprint(value)
}

func capsuleTokenFingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
