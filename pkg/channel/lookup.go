package channel

import (
	"context"
	"errors"
	"fmt"

	"github.com/kyma-project/lifecycle-manager/pkg/remote"

	"github.com/Masterminds/semver/v3"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"

	operatorv1beta1 "github.com/kyma-project/lifecycle-manager/api/v1beta1"
	"github.com/kyma-project/lifecycle-manager/pkg/log"
)

var (
	ErrTemplateNotIdentified            = errors.New("no unique template could be identified")
	ErrNotDefaultChannelAllowed         = errors.New("specifying no default channel is not allowed")
	ErrNoTemplatesInListResult          = errors.New("no templates were found during listing")
	ErrInvalidRemoteModuleConfiguration = errors.New("invalid remote module template configuration")
)

type ModuleTemplate struct {
	*operatorv1beta1.ModuleTemplate
	Outdated bool
}

type ModuleTemplatesByModuleName map[string]*ModuleTemplate

func GetTemplates(ctx context.Context, kymaClient client.Reader, kyma *operatorv1beta1.Kyma,
) (ModuleTemplatesByModuleName, error) {
	logger := ctrlLog.FromContext(ctx)
	templates := make(ModuleTemplatesByModuleName)

	var runtimeClient client.Reader
	if kyma.Spec.Sync.Enabled {
		runtimeClient = remote.SyncContextFromContext(ctx).RuntimeClient
	}

	for _, module := range kyma.Spec.Modules {
		var template *ModuleTemplate
		var err error

		switch {
		case module.RemoteModuleTemplateRef == "":
			template, err = NewTemplateLookup(kymaClient, module, kyma.Spec.Channel).WithContext(ctx)
		case kyma.Spec.Sync.Enabled:
			originalModuleName := module.Name
			module.Name = module.RemoteModuleTemplateRef // To search template with the Remote Ref
			template, err = NewTemplateLookup(runtimeClient, module, kyma.Spec.Channel).WithContext(ctx)
			module.Name = originalModuleName
		default:
			return nil, fmt.Errorf("enable sync to use a remote module template for %s: %w", module.Name,
				ErrInvalidRemoteModuleConfiguration)
		}

		if err != nil {
			return nil, err
		}

		templates[module.Name] = template
	}

	CheckForOutdatedTemplates(logger, kyma, templates)

	return templates, nil
}

func CheckForOutdatedTemplates(logger logr.Logger, k *operatorv1beta1.Kyma, templates ModuleTemplatesByModuleName) {
	// in the case that the kyma spec did not change, we only have to verify
	// that all desired templates are still referenced in the latest spec generation
	for moduleName, moduleTemplate := range templates {
		for i := range k.Status.Modules {
			moduleStatus := &k.Status.Modules[i]
			if moduleStatus.FQDN == moduleName && moduleTemplate != nil {
				CheckForOutdatedTemplate(logger, moduleTemplate, moduleStatus)
			}
		}
	}
}

// CheckForOutdatedTemplate verifies if the given ModuleTemplate is outdated and sets their Outdated Flag based on
// provided Modules, provided by the Cluster as a status of the last known module state.
// It does this by looking into selected key properties:
// 1. If the generation of ModuleTemplate changes, it means the spec is outdated
// 2. If the channel of ModuleTemplate changes, it means the kyma has an old reference to a previous channel.
func CheckForOutdatedTemplate(
	logger logr.Logger, moduleTemplate *ModuleTemplate, moduleStatus *operatorv1beta1.ModuleStatus,
) {
	checkLog := logger.WithValues("module", moduleStatus.FQDN,
		"template", moduleTemplate.Name,
		"newTemplateGeneration", moduleTemplate.GetGeneration(),
		"previousTemplateGeneration", moduleStatus.Template.Generation,
		"newTemplateChannel", moduleTemplate.Spec.Channel,
		"previousTemplateChannel", moduleStatus.Channel,
	)

	// generation skews always have to be handled. We are not in need of checking downgrades here,
	// since these are catched by our validating webhook. We do not support downgrades of Versions
	// in ModuleTemplates, meaning the only way the generation can be changed is by changing the target
	// channel (valid change) or a version increase
	if moduleTemplate.GetGeneration() != moduleStatus.Template.Generation {
		checkLog.Info("outdated ModuleTemplate: generation skew")
		moduleTemplate.Outdated = true
		return
	}

	if moduleTemplate.Spec.Channel != moduleStatus.Channel {
		checkLog.Info("outdated ModuleTemplate: channel skew")

		descriptor, err := moduleTemplate.Spec.GetDescriptor()
		if err != nil {
			checkLog.Error(err, "could not handle channel skew as descriptor from template cannot be fetched")
			return
		}

		versionInTemplate, err := semver.NewVersion(descriptor.Version)
		if err != nil {
			checkLog.Error(err, "could not handle channel skew as descriptor from template contains invalid version")
			return
		}

		versionInStatus, err := semver.NewVersion(moduleStatus.Version)
		if err != nil {
			checkLog.Error(err, "could not handle channel skew as Modules contains invalid version")
			return
		}

		checkLog = checkLog.WithValues(
			"previousVersion", versionInTemplate.String(),
			"newVersion", versionInStatus.String(),
		)

		// channel skews have to be handled with more detail. If a channel is changed this means
		// that the downstream kyma might have changed its target channel for the module, meaning
		// the old moduleStatus is reflecting the previous desired state.
		// when increasing channel stability, this means we could potentially have a downgrade
		// of module versions here (fast: v2.0.0 get downgraded to regular: v1.0.0). In this
		// case we want to suspend updating the module until we reach v2.0.0 in regular, since downgrades
		// are not supported. To circumvent this, a module can be uninstalled and then reinstalled in the old channel.
		if versionInStatus.GreaterThan(versionInTemplate) {
			checkLog.Info("ignore channel skew, as a higher version of the module was previously installed")
			return
		}

		moduleTemplate.Outdated = true
	}
}

type Lookup interface {
	WithContext(ctx context.Context) (*ModuleTemplate, error)
}

func NewTemplateLookup(client client.Reader, module operatorv1beta1.Module,
	defaultChannel string,
) *TemplateLookup {
	return &TemplateLookup{
		reader:         client,
		module:         module,
		defaultChannel: defaultChannel,
	}
}

type TemplateLookup struct {
	reader         client.Reader
	module         operatorv1beta1.Module
	defaultChannel string
}

func (c *TemplateLookup) WithContext(ctx context.Context) (*ModuleTemplate, error) {
	desiredChannel := c.getDesiredChannel()

	template, err := c.getTemplate(ctx, desiredChannel)
	if err != nil {
		return nil, err
	}

	actualChannel := template.Spec.Channel

	// ModuleTemplates without a Channel are not allowed
	if actualChannel == "" {
		return nil, fmt.Errorf(
			"no channel found on template for module: %s: %w",
			c.module.Name, ErrNotDefaultChannelAllowed,
		)
	}

	logger := ctrlLog.FromContext(ctx)
	if actualChannel != c.defaultChannel {
		logger.Info(
			fmt.Sprintf(
				"using %s (instead of %s) for module %s",
				actualChannel, c.defaultChannel, c.module.Name,
			),
		)
	} else {
		logger.V(log.DebugLevel).Info(
			fmt.Sprintf(
				"using %s for module %s",
				actualChannel, c.module.Name,
			),
		)
	}

	return &ModuleTemplate{
		ModuleTemplate: template,
		Outdated:       false,
	}, nil
}

func (c *TemplateLookup) getDesiredChannel() string {
	var desiredChannel string

	switch {
	case c.module.Channel != "":
		desiredChannel = c.module.Channel
	case c.defaultChannel != "":
		desiredChannel = c.defaultChannel
	default:
		desiredChannel = operatorv1beta1.DefaultChannel
	}

	return desiredChannel
}

func (c *TemplateLookup) getTemplate(ctx context.Context, desiredChannel string) (
	*operatorv1beta1.ModuleTemplate, error,
) {
	templateList := &operatorv1beta1.ModuleTemplateList{}
	err := c.reader.List(ctx, templateList)
	if err != nil {
		return nil, err
	}

	moduleIdentifier := c.module.Name
	var filteredTemplates []operatorv1beta1.ModuleTemplate
	for _, template := range templateList.Items {
		if template.Labels[operatorv1beta1.ModuleName] == moduleIdentifier && template.Spec.Channel == desiredChannel {
			filteredTemplates = append(filteredTemplates, template)
			continue
		}
		if template.ObjectMeta.Name == moduleIdentifier && template.Spec.Channel == desiredChannel {
			filteredTemplates = append(filteredTemplates, template)
			continue
		}
		descriptor, err := template.Spec.GetDescriptor()
		if err != nil {
			return nil, fmt.Errorf("invalid ModuleTemplate descriptor: %v", err)
		}
		if descriptor.Name == moduleIdentifier && template.Spec.Channel == desiredChannel {
			filteredTemplates = append(filteredTemplates, template)
			continue
		}
	}

	if len(filteredTemplates) > 1 {
		return nil, NewMoreThanOneTemplateCandidateErr(c.module, templateList.Items)
	}
	if len(filteredTemplates) == 0 {
		return nil, fmt.Errorf("no templates found in channel %s: %w", desiredChannel,
			ErrNoTemplatesInListResult)
	}
	return &filteredTemplates[0], nil
}

func NewMoreThanOneTemplateCandidateErr(component operatorv1beta1.Module,
	candidateTemplates []operatorv1beta1.ModuleTemplate,
) error {
	candidates := make([]string, len(candidateTemplates))
	for i, candidate := range candidateTemplates {
		candidates[i] = candidate.GetName()
	}

	return fmt.Errorf("%w: more than one module template found for module: %s, candidates: %v",
		ErrTemplateNotIdentified, component.Name, candidates)
}
