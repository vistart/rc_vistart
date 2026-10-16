package order

import (
	"fmt"
	"sort"
)

// Scenario names accepted by the preset lookup.
const (
	// PresetOrder is the canonical order: verify the subject, reserve stock
	// and confirm payment, then fulfil. Stock is touched twice — reserved
	// during prepare, released to outbound during produce.
	PresetOrder = "order.standard"
	// PresetAdRegistration notifies the ad system that a registration
	// succeeded.
	PresetAdRegistration = "ad.registration"
	// PresetContactUpdate notifies CRM to move a contact to a new status.
	PresetContactUpdate = "crm.contact"
)

// Step names used by the presets.
const (
	StepVerifyIdentity  = "verify.identity"
	StepVerifyStock     = "verify.stock"
	StepVerifyPayment   = "verify.payment"
	StepProduceDelivery = "produce.delivery"
	StepStockOutbound   = "stock.outbound"

	StepAdRegistration = "ads.registration"
	StepContactUpdate  = "contact.update"
)

// Topics the presets address. A downstream registers one endpoint per topic.
const (
	TopicIdentityCheck = "identity.verify"
	TopicStockPrehold  = "inventory.stock.prehold"
	TopicPaymentVerify = "payment.verify"
	TopicDelivery      = "fulfilment.delivery"
	TopicStockOutbound = "inventory.stock.outbound"

	TopicAdRegistration = "ads.registration"
	TopicContactUpdate  = "crm.contact.update"
)

// Preset is a named step graph.
type Preset struct {
	Name        string
	Description string
	Steps       []StepSpec
}

// presets are keyed by name.
var presets = map[string]Preset{
	PresetOrder: {
		Name: PresetOrder,
		Description: "Verify identity, stock and payment, then fulfil and ship. " +
			"Stock is reserved during prepare and handed over during produce.",
		Steps: []StepSpec{
			{
				Name:  StepVerifyIdentity,
				Phase: PhasePrepare,
				Kind:  KindCallback,
				Topic: TopicIdentityCheck,
				// The result must say the subject exists, so this waits for a
				// report rather than trusting the acknowledgement.
			},
			{
				Name:  StepVerifyStock,
				Phase: PhasePrepare,
				Kind:  KindCallback,
				Topic: TopicStockPrehold,
				// Reserve stock before payment is confirmed, so an unpaid
				// order cannot consume the reservation window.
				DependsOn: []string{StepVerifyIdentity},
			},
			{
				Name:      StepVerifyPayment,
				Phase:     PhasePrepare,
				Kind:      KindCallback,
				Topic:     TopicPaymentVerify,
				DependsOn: []string{StepVerifyStock},
			},
			{
				Name:  StepProduceDelivery,
				Phase: PhaseProduce,
				Kind:  KindNotify,
				Topic: TopicDelivery,
				// No cross-phase dependency is declared: reaching the produce
				// phase already means every prepare check succeeded.
			},
			{
				Name:      StepStockOutbound,
				Phase:     PhaseProduce,
				Kind:      KindNotify,
				Topic:     TopicStockOutbound,
				DependsOn: []string{StepProduceDelivery},
			},
		},
	},
	PresetAdRegistration: {
		Name:        PresetAdRegistration,
		Description: "Tell the ad system a registration succeeded.",
		Steps: []StepSpec{
			{Name: StepAdRegistration, Phase: PhasePrepare, Kind: KindNotify, Topic: TopicAdRegistration},
		},
	},
	PresetContactUpdate: {
		Name:        PresetContactUpdate,
		Description: "Move a CRM contact to the status implied by a successful payment.",
		Steps: []StepSpec{
			{Name: StepContactUpdate, Phase: PhasePrepare, Kind: KindNotify, Topic: TopicContactUpdate},
		},
	},
}

// LookupPreset returns the named preset.
func LookupPreset(name string) (Preset, error) {
	p, ok := presets[name]
	if !ok {
		return Preset{}, fmt.Errorf("%w: unknown preset %q", ErrInvalid, name)
	}
	return p, nil
}

// ListPresets returns the known presets, sorted by name.
func ListPresets() []Preset {
	out := make([]Preset, 0, len(presets))
	for _, p := range presets {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// StepsFor returns a copy of the preset's step graph, safe for the caller to
// modify.
func (p Preset) StepsFor() []StepSpec {
	out := make([]StepSpec, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = s
		if s.DependsOn != nil {
			out[i].DependsOn = append([]string(nil), s.DependsOn...)
		}
	}
	return out
}

// StandardSteps returns the canonical order graph.
func StandardSteps() []StepSpec { return presets[PresetOrder].StepsFor() }
