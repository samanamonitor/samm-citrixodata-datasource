package plugin

type queryModel struct {
	EntitySet        entitySet         `json:"entitySet"`
	TimeProperty     *property         `json:"timeProperty"`
	Properties       []property        `json:"properties"`
	FilterConditions []filterCondition `json:"filterConditions"`
	Count            bool              `json:"count,omitempty"`
}

type schema struct {
	EntityTypes map[string]entityType `json:"entityTypes"`
	EntitySets  map[string]entitySet  `json:"entitySets"`
}

type entityType struct {
	Name          string     `json:"name"`
	QualifiedName string     `json:"qualifiedName"`
	Properties    []property `json:"properties"`
}

type entitySet struct {
	Name       string `json:"name"`
	EntityType string `json:"entityType"`
}

type property struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type filterCondition struct {
	Property property `json:"property"`
	Operator string   `json:"operator"`
	Value    string   `json:"value"`
}
