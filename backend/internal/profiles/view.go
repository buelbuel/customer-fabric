package profiles

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"
)

// ViewLogic names the rule set a CustomerView was built with. It selects no winner: every visible
// value is returned with its provenance. v2 discloses only defined attributes under an access policy.
const ViewLogic = "governed-verified-records-multi-value-v2"

// RecordStatus says whether a linked record contributes values to a view.
type RecordStatus string

const (
	RecordIncluded            RecordStatus = "included"
	RecordRestricted          RecordStatus = "restricted"
	RecordNotSourceOwned      RecordStatus = "not_source_owned"
	RecordMultipleIdentifiers RecordStatus = "multiple_identifiers"
)

// AttributeEvidence is an attribute's owner and that owner's newest accepted write.
type AttributeEvidence struct {
	Owner             string
	MutationID        string
	At                time.Time
	AttributeRevision *int32
}

// Governance is the current definition and access policy of each governed attribute.
type Governance struct {
	Definitions map[string]AttributeDefinition
	Policies    map[string]AccessPolicy
}

// discloses denies by default: an undefined attribute, or one without a policy that names both the
// reader and the owning source, is never readable.
func (g Governance) discloses(reader Reader, source, path string) bool {
	_, defined := g.Definitions[path]
	policy, governed := g.Policies[path]
	return defined && governed && policy.Allows(reader, source)
}

// LinkedRecord is one verified association and the source-owned profile it points to.
type LinkedRecord struct {
	AssociationID   string
	Source          string
	LinkedAt        time.Time
	RecordProfileID string
	Version         int64
	UpdatedAt       time.Time
	SourceOwned     bool
	Identifiers     int64
	Restricted      bool
	Attributes      map[string]json.RawMessage
	Evidence        map[string]AttributeEvidence
}

type CustomerView struct {
	AnchorProfileID string          `json:"anchor_profile_id"`
	Logic           string          `json:"logic"`
	Reader          Reader          `json:"reader"`
	Attributes      []ViewAttribute `json:"attributes"`
	Records         []ViewRecord    `json:"records"`
}

type ViewAttribute struct {
	Path               string        `json:"path"`
	Type               AttributeType `json:"type"`
	DefinitionRevision int32         `json:"definition_revision"`
	PolicyRevision     int32         `json:"policy_revision"`
	Conflict           bool          `json:"conflict"`
	Values             []ViewValue   `json:"values"`
}

type ViewValue struct {
	Value           json.RawMessage `json:"value"`
	Source          string          `json:"source"`
	AssociationID   string          `json:"association_id"`
	RecordProfileID string          `json:"record_profile_id"`
	MutationID      string          `json:"mutation_id"`
	WrittenAt       time.Time       `json:"written_at"`
	// AttributeRevision is the definition revision the value was validated under; absent for a value
	// written before the attribute was defined, which was checked when the definition was adopted.
	AttributeRevision *int32 `json:"attribute_revision,omitempty"`
}

// ViewRecord describes a contributing record. A record that is not included discloses only its
// association metadata and status.
type ViewRecord struct {
	AssociationID      string       `json:"association_id"`
	Source             string       `json:"source"`
	LinkedAt           time.Time    `json:"linked_at"`
	Status             RecordStatus `json:"status"`
	RecordProfileID    string       `json:"record_profile_id,omitempty"`
	ProfileVersion     int64        `json:"profile_version,omitempty"`
	UpdatedAt          *time.Time   `json:"updated_at,omitempty"`
	WithheldAttributes int          `json:"withheld_attributes"`
}

func (r LinkedRecord) status() RecordStatus {
	switch {
	case r.Restricted:
		return RecordRestricted
	case !r.SourceOwned:
		return RecordNotSourceOwned
	case r.Identifiers != 1:
		// Attributes belong to the profile, not to one identifier, and each identifier can be
		// linked to a different customer, so no value can be attributed to this association.
		return RecordMultipleIdentifiers
	}
	return RecordIncluded
}

// BuildCustomerView derives a read-only view for one reader. A value is disclosed only if governance
// allows that reader to see the attribute from its record's source; anything else is withheld. It also
// needs its record's source as owner and an accepted write by that owner, so a linked record never
// discloses another source's attribute.
func BuildCustomerView(anchorID string, reader Reader, records []LinkedRecord, governance Governance) CustomerView {
	view := CustomerView{AnchorProfileID: anchorID, Logic: ViewLogic, Reader: reader, Attributes: []ViewAttribute{}, Records: []ViewRecord{}}
	values := map[string][]ViewValue{}
	for _, r := range records {
		out := ViewRecord{AssociationID: r.AssociationID, Source: r.Source, LinkedAt: r.LinkedAt, Status: r.status()}
		if out.Status == RecordIncluded {
			updated := r.UpdatedAt
			out.RecordProfileID, out.ProfileVersion, out.UpdatedAt = r.RecordProfileID, r.Version, &updated
			for path, value := range r.Attributes {
				evidence, known := r.Evidence[path]
				if !known || evidence.Owner != r.Source || !governance.discloses(reader, r.Source, path) {
					out.WithheldAttributes++
					continue
				}
				values[path] = append(values[path], ViewValue{Value: value, Source: r.Source, AssociationID: r.AssociationID,
					RecordProfileID: r.RecordProfileID, MutationID: evidence.MutationID, WrittenAt: evidence.At,
					AttributeRevision: evidence.AttributeRevision})
			}
		}
		view.Records = append(view.Records, out)
	}
	for path, list := range values {
		sort.Slice(list, func(i, j int) bool {
			if list[i].Source != list[j].Source {
				return list[i].Source < list[j].Source
			}
			return list[i].AssociationID < list[j].AssociationID
		})
		conflict := false
		for _, v := range list[1:] {
			// Stored JSON text is compared, so 1 and 1.0 report as a conflict. Both stay visible.
			conflict = conflict || !bytes.Equal(v.Value, list[0].Value)
		}
		definition := governance.Definitions[path]
		view.Attributes = append(view.Attributes, ViewAttribute{Path: path, Type: definition.Type, DefinitionRevision: definition.Revision,
			PolicyRevision: governance.Policies[path].Revision, Conflict: conflict, Values: list})
	}
	sort.Slice(view.Attributes, func(i, j int) bool { return view.Attributes[i].Path < view.Attributes[j].Path })
	return view
}
