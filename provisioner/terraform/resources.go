package terraform

import (
	"strings"

	"github.com/awalterschulze/gographviz"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/mitchellh/mapstructure"
	"golang.org/x/xerrors"

	"github.com/coder/coder/provisioner"
	"github.com/coder/coder/provisionersdk/proto"
)

// A mapping of attributes on the "coder_agent" resource.
type agentAttributes struct {
	Auth                     string            `mapstructure:"auth"`
	OperatingSystem          string            `mapstructure:"os"`
	Architecture             string            `mapstructure:"arch"`
	Directory                string            `mapstructure:"dir"`
	ID                       string            `mapstructure:"id"`
	Token                    string            `mapstructure:"token"`
	Env                      map[string]string `mapstructure:"env"`
	StartupScript            string            `mapstructure:"startup_script"`
	ConnectionTimeoutSeconds int32             `mapstructure:"connection_timeout"`
	TroubleshootingURL       string            `mapstructure:"troubleshooting_url"`
}

// A mapping of attributes on the "coder_app" resource.
type agentAppAttributes struct {
	AgentID string `mapstructure:"agent_id"`
	// Slug is required in terraform, but to avoid breaking existing users we
	// will default to the resource name if it is not specified.
	Slug        string `mapstructure:"slug"`
	DisplayName string `mapstructure:"display_name"`
	// Name is deprecated in favor of DisplayName.
	Name        string                     `mapstructure:"name"`
	Icon        string                     `mapstructure:"icon"`
	URL         string                     `mapstructure:"url"`
	Command     string                     `mapstructure:"command"`
	Share       string                     `mapstructure:"share"`
	Subdomain   bool                       `mapstructure:"subdomain"`
	Healthcheck []appHealthcheckAttributes `mapstructure:"healthcheck"`
}

// A mapping of attributes on the "healthcheck" resource.
type appHealthcheckAttributes struct {
	URL       string `mapstructure:"url"`
	Interval  int32  `mapstructure:"interval"`
	Threshold int32  `mapstructure:"threshold"`
}

// A mapping of attributes on the "coder_metadata" resource.
type metadataAttributes struct {
	ResourceID string         `mapstructure:"resource_id"`
	Hide       bool           `mapstructure:"hide"`
	Icon       string         `mapstructure:"icon"`
	DailyCost  int32          `mapstructure:"daily_cost"`
	Items      []metadataItem `mapstructure:"item"`
}

type metadataItem struct {
	Key       string `mapstructure:"key"`
	Value     string `mapstructure:"value"`
	Sensitive bool   `mapstructure:"sensitive"`
	IsNull    bool   `mapstructure:"is_null"`
}

// ConvertResources consumes Terraform state and a GraphViz representation produced by
// `terraform graph` to produce resources consumable by Coder.
// nolint:gocyclo
func ConvertResources(module *tfjson.StateModule, rawGraph string) ([]*proto.Resource, error) {
	parsedGraph, err := gographviz.ParseString(rawGraph)
	if err != nil {
		return nil, xerrors.Errorf("parse graph: %w", err)
	}
	graph, err := gographviz.NewAnalysedGraph(parsedGraph)
	if err != nil {
		return nil, xerrors.Errorf("analyze graph: %w", err)
	}

	resources := make([]*proto.Resource, 0)
	resourceAgents := map[string][]*proto.Agent{}

	// Indexes Terraform resources by their label and ID.
	// The label is what "terraform graph" uses to reference nodes, and the ID
	// is used by "coder_metadata" resources to refer to their targets. (The ID
	// field is only available when reading a state file, and not when reading a
	// plan file.)
	tfResourceByLabel := map[string]*tfjson.StateResource{}
	resourceLabelByID := map[string]string{}
	var findTerraformResources func(mod *tfjson.StateModule)
	findTerraformResources = func(mod *tfjson.StateModule) {
		for _, module := range mod.ChildModules {
			findTerraformResources(module)
		}
		for _, resource := range mod.Resources {
			label := convertAddressToLabel(resource.Address)
			// index by label
			tfResourceByLabel[label] = resource
			// index by ID, if it exists
			id, ok := resource.AttributeValues["id"]
			if ok {
				idString, ok := id.(string)
				if ok {
					resourceLabelByID[idString] = label
				}
			}
		}
	}
	findTerraformResources(module)

	// Find all agents!
	for _, tfResource := range tfResourceByLabel {
		if tfResource.Type != "coder_agent" {
			continue
		}
		var attrs agentAttributes
		err = mapstructure.Decode(tfResource.AttributeValues, &attrs)
		if err != nil {
			return nil, xerrors.Errorf("decode agent attributes: %w", err)
		}
		agent := &proto.Agent{
			Name:                     tfResource.Name,
			Id:                       attrs.ID,
			Env:                      attrs.Env,
			StartupScript:            attrs.StartupScript,
			OperatingSystem:          attrs.OperatingSystem,
			Architecture:             attrs.Architecture,
			Directory:                attrs.Directory,
			ConnectionTimeoutSeconds: attrs.ConnectionTimeoutSeconds,
			TroubleshootingUrl:       attrs.TroubleshootingURL,
		}
		switch attrs.Auth {
		case "token":
			agent.Auth = &proto.Agent_Token{
				Token: attrs.Token,
			}
		default:
			// If token authentication isn't specified,
			// assume instance auth. It's our only other
			// authentication type!
			agent.Auth = &proto.Agent_InstanceId{}
		}

		// The label is used to find the graph node!
		agentLabel := convertAddressToLabel(tfResource.Address)

		var agentNode *gographviz.Node
		for _, node := range graph.Nodes.Lookup {
			// The node attributes surround the label with quotes.
			if strings.Trim(node.Attrs["label"], `"`) != agentLabel {
				continue
			}
			agentNode = node
			break
		}
		if agentNode == nil {
			return nil, xerrors.Errorf("couldn't find node on graph: %q", agentLabel)
		}

		var agentResource *graphResource
		for _, resource := range findResourcesInGraph(graph, tfResourceByLabel, agentNode.Name, 0, true) {
			if agentResource == nil {
				// Default to the first resource because we have nothing to compare!
				agentResource = resource
				continue
			}
			if resource.Depth < agentResource.Depth {
				// There's a closer resource!
				agentResource = resource
				continue
			}
			if resource.Depth == agentResource.Depth && resource.Label < agentResource.Label {
				agentResource = resource
				continue
			}
		}

		if agentResource == nil {
			continue
		}

		agents, exists := resourceAgents[agentResource.Label]
		if !exists {
			agents = make([]*proto.Agent, 0)
		}
		agents = append(agents, agent)
		resourceAgents[agentResource.Label] = agents
	}

	// Manually associate agents with instance IDs.
	for _, resource := range tfResourceByLabel {
		if resource.Type != "coder_agent_instance" {
			continue
		}
		agentIDRaw, valid := resource.AttributeValues["agent_id"]
		if !valid {
			continue
		}
		agentID, valid := agentIDRaw.(string)
		if !valid {
			continue
		}
		instanceIDRaw, valid := resource.AttributeValues["instance_id"]
		if !valid {
			continue
		}
		instanceID, valid := instanceIDRaw.(string)
		if !valid {
			continue
		}

		for _, agents := range resourceAgents {
			for _, agent := range agents {
				if agent.Id != agentID {
					continue
				}
				agent.Auth = &proto.Agent_InstanceId{
					InstanceId: instanceID,
				}
				break
			}
		}
	}

	// Associate Apps with agents.
	appSlugs := make(map[string]struct{})
	for _, resource := range tfResourceByLabel {
		if resource.Type != "coder_app" {
			continue
		}

		var attrs agentAppAttributes
		err = mapstructure.Decode(resource.AttributeValues, &attrs)
		if err != nil {
			return nil, xerrors.Errorf("decode app attributes: %w", err)
		}

		// Default to the resource name if none is set!
		if attrs.Slug == "" {
			attrs.Slug = resource.Name
		}
		if attrs.DisplayName == "" {
			if attrs.Name != "" {
				// Name is deprecated but still accepted.
				attrs.DisplayName = attrs.Name
			} else {
				attrs.DisplayName = attrs.Slug
			}
		}

		if !provisioner.AppSlugRegex.MatchString(attrs.Slug) {
			return nil, xerrors.Errorf("invalid app slug %q, please update your coder/coder provider to the latest version and specify the slug property on each coder_app", attrs.Slug)
		}

		if _, exists := appSlugs[attrs.Slug]; exists {
			return nil, xerrors.Errorf("duplicate app slug, they must be unique per template: %q", attrs.Slug)
		}
		appSlugs[attrs.Slug] = struct{}{}

		var healthcheck *proto.Healthcheck
		if len(attrs.Healthcheck) != 0 {
			healthcheck = &proto.Healthcheck{
				Url:       attrs.Healthcheck[0].URL,
				Interval:  attrs.Healthcheck[0].Interval,
				Threshold: attrs.Healthcheck[0].Threshold,
			}
		}

		sharingLevel := proto.AppSharingLevel_OWNER
		switch strings.ToLower(attrs.Share) {
		case "owner":
			sharingLevel = proto.AppSharingLevel_OWNER
		case "authenticated":
			sharingLevel = proto.AppSharingLevel_AUTHENTICATED
		case "public":
			sharingLevel = proto.AppSharingLevel_PUBLIC
		}

		for _, agents := range resourceAgents {
			for _, agent := range agents {
				// Find agents with the matching ID and associate them!
				if agent.Id != attrs.AgentID {
					continue
				}
				agent.Apps = append(agent.Apps, &proto.App{
					Slug:         attrs.Slug,
					DisplayName:  attrs.DisplayName,
					Command:      attrs.Command,
					Url:          attrs.URL,
					Icon:         attrs.Icon,
					Subdomain:    attrs.Subdomain,
					SharingLevel: sharingLevel,
					Healthcheck:  healthcheck,
				})
			}
		}
	}

	// Associate metadata blocks with resources.
	resourceMetadata := map[string][]*proto.Resource_Metadata{}
	resourceHidden := map[string]bool{}
	resourceIcon := map[string]string{}
	resourceCost := map[string]int32{}

	for _, resource := range tfResourceByLabel {
		if resource.Type != "coder_metadata" {
			continue
		}
		var attrs metadataAttributes
		err = mapstructure.Decode(resource.AttributeValues, &attrs)
		if err != nil {
			return nil, xerrors.Errorf("decode metadata attributes: %w", err)
		}

		var targetLabel string
		// This occurs in a plan, because there is no resource ID.
		// We attempt to find the closest node, just so we can hide it from the UI.
		if attrs.ResourceID == "" {
			resourceLabel := convertAddressToLabel(resource.Address)

			var attachedNode *gographviz.Node
			for _, node := range graph.Nodes.Lookup {
				// The node attributes surround the label with quotes.
				if strings.Trim(node.Attrs["label"], `"`) != resourceLabel {
					continue
				}
				attachedNode = node
				break
			}
			if attachedNode == nil {
				continue
			}
			var attachedResource *graphResource
			for _, resource := range findResourcesInGraph(graph, tfResourceByLabel, attachedNode.Name, 0, false) {
				if attachedResource == nil {
					// Default to the first resource because we have nothing to compare!
					attachedResource = resource
					continue
				}
				if resource.Depth < attachedResource.Depth {
					// There's a closer resource!
					attachedResource = resource
					continue
				}
				if resource.Depth == attachedResource.Depth && resource.Label < attachedResource.Label {
					attachedResource = resource
					continue
				}
			}
			if attachedResource == nil {
				continue
			}
			targetLabel = attachedResource.Label
		}
		if targetLabel == "" {
			targetLabel = resourceLabelByID[attrs.ResourceID]
		}
		if targetLabel == "" {
			continue
		}

		resourceHidden[targetLabel] = attrs.Hide
		resourceIcon[targetLabel] = attrs.Icon
		resourceCost[targetLabel] = attrs.DailyCost
		for _, item := range attrs.Items {
			resourceMetadata[targetLabel] = append(resourceMetadata[targetLabel],
				&proto.Resource_Metadata{
					Key:       item.Key,
					Value:     item.Value,
					Sensitive: item.Sensitive,
					IsNull:    item.IsNull,
				})
		}
	}

	for _, resource := range tfResourceByLabel {
		if resource.Mode == tfjson.DataResourceMode {
			continue
		}
		if resource.Type == "coder_agent" || resource.Type == "coder_agent_instance" || resource.Type == "coder_app" || resource.Type == "coder_metadata" {
			continue
		}
		label := convertAddressToLabel(resource.Address)

		agents, exists := resourceAgents[label]
		if exists {
			applyAutomaticInstanceID(resource, agents)
		}

		resources = append(resources, &proto.Resource{
			Name:         resource.Name,
			Type:         resource.Type,
			Agents:       agents,
			Metadata:     resourceMetadata[label],
			Hide:         resourceHidden[label],
			Icon:         resourceIcon[label],
			DailyCost:    resourceCost[label],
			InstanceType: applyInstanceType(resource),
		})
	}

	return resources, nil
}

// convertAddressToLabel returns the Terraform address without the count
// specifier. eg. "module.ec2_dev.ec2_instance.dev[0]" becomes "module.ec2_dev.ec2_instance.dev"
func convertAddressToLabel(address string) string {
	return strings.Split(address, "[")[0]
}

type graphResource struct {
	Label string
	Depth uint
}

// applyInstanceType sets the instance type on an agent if it matches
// one of the special resource types that we track.
func applyInstanceType(resource *tfjson.StateResource) string {
	key, isValid := map[string]string{
		"google_compute_instance":         "machine_type",
		"aws_instance":                    "instance_type",
		"aws_spot_instance_request":       "instance_type",
		"azurerm_linux_virtual_machine":   "size",
		"azurerm_windows_virtual_machine": "size",
	}[resource.Type]
	if !isValid {
		return ""
	}

	instanceTypeRaw, isValid := resource.AttributeValues[key]
	if !isValid {
		return ""
	}
	instanceType, isValid := instanceTypeRaw.(string)
	if !isValid {
		return ""
	}
	return instanceType
}

// applyAutomaticInstanceID checks if the resource is one of a set of *magical* IDs
// that automatically index their identifier for automatic authentication.
func applyAutomaticInstanceID(resource *tfjson.StateResource, agents []*proto.Agent) {
	// These resource types are for automatically associating an instance ID
	// with an agent for authentication.
	key, isValid := map[string]string{
		"google_compute_instance":         "instance_id",
		"aws_instance":                    "id",
		"aws_spot_instance_request":       "spot_instance_id",
		"azurerm_linux_virtual_machine":   "virtual_machine_id",
		"azurerm_windows_virtual_machine": "virtual_machine_id",
	}[resource.Type]
	if !isValid {
		return
	}

	// The resource type doesn't support
	// automatically setting the instance ID.
	instanceIDRaw, isValid := resource.AttributeValues[key]
	if !isValid {
		return
	}
	instanceID, isValid := instanceIDRaw.(string)
	if !isValid {
		return
	}
	for _, agent := range agents {
		// Didn't use instance identity.
		if agent.GetToken() != "" {
			continue
		}
		if agent.GetInstanceId() != "" {
			// If an instance ID is manually specified, do not override!
			continue
		}

		agent.Auth = &proto.Agent_InstanceId{
			InstanceId: instanceID,
		}
	}
}

// findResourcesInGraph traverses directionally in a graph until a resource is found,
// then it stores the depth it was found at, and continues working up the tree.
// nolint:revive
func findResourcesInGraph(graph *gographviz.Graph, tfResourceByLabel map[string]*tfjson.StateResource, nodeName string, currentDepth uint, up bool) []*graphResource {
	graphResources := make([]*graphResource, 0)
	mapping := graph.Edges.DstToSrcs
	if !up {
		mapping = graph.Edges.SrcToDsts
	}
	for destination := range mapping[nodeName] {
		destinationNode := graph.Nodes.Lookup[destination]
		// Work our way up the tree!
		graphResources = append(graphResources, findResourcesInGraph(graph, tfResourceByLabel, destinationNode.Name, currentDepth+1, up)...)

		destinationLabel, exists := destinationNode.Attrs["label"]
		if !exists {
			continue
		}
		destinationLabel = strings.Trim(destinationLabel, `"`)
		resource, exists := tfResourceByLabel[destinationLabel]
		if !exists {
			continue
		}
		// Data sources cannot be associated with agents for now!
		if resource.Mode != tfjson.ManagedResourceMode {
			continue
		}
		// Don't associate Coder resources with other Coder resources!
		if strings.HasPrefix(resource.Type, "coder_") {
			continue
		}
		graphResources = append(graphResources, &graphResource{
			Label: destinationLabel,
			Depth: currentDepth,
		})
	}

	return graphResources
}
