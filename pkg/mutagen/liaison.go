package mutagen

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/docker/cli/cli/command"

	"github.com/docker/docker/client"

	"github.com/compose-spec/compose-go/types"

	"github.com/docker/compose/v2/pkg/api"

	"github.com/mitchellh/mapstructure"

	"github.com/mutagen-io/mutagen/pkg/forwarding"
	"github.com/mutagen-io/mutagen/pkg/mutagen"
	"github.com/mutagen-io/mutagen/pkg/selection"
	forwardingsvc "github.com/mutagen-io/mutagen/pkg/service/forwarding"
	synchronizationsvc "github.com/mutagen-io/mutagen/pkg/service/synchronization"
	"github.com/mutagen-io/mutagen/pkg/synchronization"
	"github.com/mutagen-io/mutagen/pkg/url"
	forwardingurl "github.com/mutagen-io/mutagen/pkg/url/forwarding"
)

// Liaison is the interface point between Compose and Mutagen. Its zero value is
// initialized and ready to use. It implements the Compose service API. It is a
// single-use type, is not safe for concurrent usage, and its Shutdown method
// should be invoked when usage is complete.
type Liaison struct {
	// dockerCLI is the associated Docker CLI instance.
	dockerCLI command.Cli
	// composeService is the underlying Compose service.
	composeService api.Service
	// forwarding are the forwarding session specifications. This map is
	// initialized by calling processProject.
	forwarding map[string]*forwardingsvc.CreationSpecification
	// synchronization are the synchronization session specifications. This map
	// is initialized by calling processProject.
	synchronization map[string]*synchronizationsvc.CreationSpecification
}

// RegisterDockerCLI registers the associated Docker CLI instance.
func (l *Liaison) RegisterDockerCLI(cli command.Cli) {
	l.dockerCLI = cli
}

// DockerClient returns a Mutagen-aware version of the Docker API client. This
// method must only be called after the associated Docker CLI (registered with
// RegisterDockerCLI) can return a valid API client via its Client method.
func (l *Liaison) DockerClient() client.APIClient {
	return &dockerAPIClient{l, l.dockerCLI.Client()}
}

// RegisterComposeService registers the underlying Compose service. The Compose
// service must be initialized using the Docker API client returned by the
// liaison's DockerClient method.
func (l *Liaison) RegisterComposeService(service api.Service) {
	l.composeService = service
}

// ComposeService returns a Mutagen-aware version of the Compose Service API.
// This function must only be called after a Compose service has been registered
// with RegisterComposeService.
func (l *Liaison) ComposeService() api.Service {
	return &composeService{l, l.composeService}
}

// processProject loads Mutagen configuration from the specified project, adds
// the Mutagen sidecar service to the project, and sets project dependencies
// accordingly. If project is nil, this method is a no-op and returns nil. This
// method must only be called after the associated Docker CLI (registered with
// RegisterDockerCLI) can return a valid API client via its Client method.
func (l *Liaison) processProject(project *types.Project) error {
	// If the project is nil, then there's nothing to process. In this case,
	// containers are typically operated on by project name and label selection,
	// so there's no need to modify the project because the Mutagen sidecar
	// service will still be affected by the corresponding operation.
	if project == nil {
		return nil
	}

	// Check for service name conflicts with explicitly-defined services.
	for _, service := range project.Services {
		if service.Name == sidecarServiceName {
			return fmt.Errorf("Mutagen sidecar service name (%s) conflicts with existing service", sidecarServiceName)
		}
	}

	// Query daemon metadata.
	daemonMetadata, err := l.dockerCLI.Client().Info(context.Background())
	if err != nil {
		return fmt.Errorf("unable to query daemon metadata: %w", err)
	}

	// Extract and decode the Mutagen extension section. If none is present,
	// then we'll just create an empty configuration, but we'll still proceed
	// with injecting the Mutagen sidecar service into the project in order to
	// ensure that it is affected by Compose. This is particularly important for
	// the "down" operation, where, in the event that someone had deleted the
	// x-mutagen extension section after running "up", the Mutagen sidecar
	// service would be seen as an orphan container.
	sessions := &configuration{}
	if xMutagen, ok := project.Extensions["x-mutagen"]; ok {
		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.TextUnmarshallerHookFunc(),
				boolToIgnoreVCSModeHookFunc(),
			),
			ErrorUnused: true,
			Result:      sessions,
		})
		if err != nil {
			return fmt.Errorf("unable to create configuration decoder: %w", err)
		} else if err = decoder.Decode(xMutagen); err != nil {
			return fmt.Errorf("unable to decode x-mutagen section: %w", err)
		}
	}

	// Extract default forwarding session parameters.
	defaultConfigurationForwarding := &forwarding.Configuration{}
	defaultConfigurationSource := &forwarding.Configuration{}
	defaultConfigurationDestination := &forwarding.Configuration{}
	if defaults, ok := sessions.Forwarding["defaults"]; ok {
		if defaults.Source != "" {
			return errors.New("source URL not allowed in default forwarding configuration")
		} else if defaults.Destination != "" {
			return errors.New("destination URL not allowed in default forwarding configuration")
		}
		defaultConfigurationForwarding = defaults.Configuration.Configuration()
		if err := defaultConfigurationForwarding.EnsureValid(false); err != nil {
			return fmt.Errorf("invalid default forwarding configuration: %w", err)
		}
		defaultConfigurationSource = defaults.ConfigurationSource.Configuration()
		if err := defaultConfigurationSource.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid default forwarding source configuration: %w", err)
		}
		defaultConfigurationDestination = defaults.ConfigurationDestination.Configuration()
		if err := defaultConfigurationDestination.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid default forwarding destination configuration: %w", err)
		}
		delete(sessions.Forwarding, "defaults")
	}

	// Extract and validate synchronization defaults.
	defaultConfigurationSynchronization := &synchronization.Configuration{}
	defaultConfigurationAlpha := &synchronization.Configuration{}
	defaultConfigurationBeta := &synchronization.Configuration{}
	if defaults, ok := sessions.Synchronization["defaults"]; ok {
		if defaults.Alpha != "" {
			return errors.New("alpha URL not allowed in default synchronization configuration")
		} else if defaults.Beta != "" {
			return errors.New("beta URL not allowed in default synchronization configuration")
		}
		defaultConfigurationSynchronization = defaults.Configuration.Configuration()
		if err := defaultConfigurationSynchronization.EnsureValid(false); err != nil {
			return fmt.Errorf("invalid default synchronization configuration: %w", err)
		}
		defaultConfigurationAlpha = defaults.ConfigurationAlpha.Configuration()
		if err := defaultConfigurationAlpha.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid default synchronization alpha configuration: %w", err)
		}
		defaultConfigurationBeta = defaults.ConfigurationBeta.Configuration()
		if err := defaultConfigurationBeta.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid default synchronization beta configuration: %w", err)
		}
		delete(sessions.Synchronization, "defaults")
	}

	// Validate forwarding configurations, convert them to session creation
	// specifications, and extract network dependencies for the Mutagen service.
	forwardingSpecifications := make(map[string]*forwardingsvc.CreationSpecification)
	networkDependencies := make(map[string]*types.ServiceNetworkConfig)
	for name, session := range sessions.Forwarding {
		// Verify that the name is valid.
		if err := selection.EnsureNameValid(name); err != nil {
			return fmt.Errorf("invalid forwarding session name (%s): %w", name, err)
		}

		// Parse and validate the source URL. At the moment, we only allow local
		// URLs as forwarding sources since this is the primary use case with
		// Docker Compose. Supporting reverse forwarding is somewhat ill-defined
		// and would require the injection of additional services to intercept
		// traffic (though we may support this in the future). We also avoid
		// other protocols (such as SSH and Docker) since they're likely to be
		// confusing and error-prone (especially raw Docker URLs referencing
		// containers in this project that won't play nicely with container
		// startup ordering). Finally, we only support TCP-based endpoints since
		// they constitute the primary use case with Docker Compose and because
		// other protocols would likely be error-prone and require
		// project-relative path resolution.
		if isNetworkURL(session.Source) {
			return fmt.Errorf("network URL (%s) not allowed as forwarding source", session.Source)
		}
		sourceURL, err := url.Parse(session.Source, url.Kind_Forwarding, true)
		if err != nil {
			return fmt.Errorf("unable to parse forwarding source URL (%s): %w", session.Source, err)
		} else if sourceURL.Protocol != url.Protocol_Local {
			return errors.New("only local URLs allowed as forwarding sources")
		} else if protocol, _, err := forwardingurl.Parse(sourceURL.Path); err != nil {
			panic("forwarding URL failed to reparse")
		} else if !isTCPForwardingProtocol(protocol) {
			return fmt.Errorf("non-TCP-based forwarding endpoint (%s) unsupported", sourceURL.Path)
		}

		// Parse and validate the destination URL. At the moment, we only allow
		// network pseudo-URLs (with TCP-based endpoints) as forwarding
		// destinations for the reasons outlined above. The parseNetworkURL will
		// enforce that a TCP-based forwarding endpoint is used.
		if !isNetworkURL(session.Destination) {
			return fmt.Errorf("forwarding destination (%s) should be a network URL", session.Destination)
		}
		destinationURL, network, err := parseNetworkURL(session.Destination)
		if err != nil {
			return fmt.Errorf("unable to parse forwarding destination URL (%s): %w", session.Destination, err)
		}
		networkDependencies[network] = nil

		// Compute the session configuration.
		configuration := session.Configuration.Configuration()
		if err := configuration.EnsureValid(false); err != nil {
			return fmt.Errorf("invalid forwarding session configuration for %s: %w", name, err)
		}
		configuration = forwarding.MergeConfigurations(defaultConfigurationForwarding, configuration)

		// Compute the source-specific configuration.
		sourceConfiguration := session.ConfigurationSource.Configuration()
		if err := sourceConfiguration.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid forwarding session source configuration for %s: %w", name, err)
		}
		sourceConfiguration = forwarding.MergeConfigurations(defaultConfigurationSource, sourceConfiguration)

		// Compute the destination-specific configuration.
		destinationConfiguration := session.ConfigurationDestination.Configuration()
		if err := destinationConfiguration.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid forwarding session destination configuration for %s: %w", name, err)
		}
		destinationConfiguration = forwarding.MergeConfigurations(defaultConfigurationDestination, destinationConfiguration)

		// Record the specification.
		forwardingSpecifications[name] = &forwardingsvc.CreationSpecification{
			Source:                   sourceURL,
			Destination:              destinationURL,
			Configuration:            configuration,
			ConfigurationSource:      sourceConfiguration,
			ConfigurationDestination: destinationConfiguration,
			Name:                     name,
		}
	}

	// Validate synchronization configurations, convert them to session creation
	// specifications, and extract volume dependencies for the Mutagen service.
	synchronizationSpecifications := make(map[string]*synchronizationsvc.CreationSpecification)
	volumeDependencies := make(map[string]bool)
	for name, session := range sessions.Synchronization {
		// Verify that the name is valid.
		if err := selection.EnsureNameValid(name); err != nil {
			return fmt.Errorf("invalid synchronization session name (%s): %v", name, err)
		}

		// Enforce that exactly one of the session URLs is a volume URL. At the
		// moment, we only support synchronization sessions where one of the
		// URLs is local the other is a volume URL. We'll check that the
		// non-volume URL is local when parsing. We could support other protocol
		// combinations for synchronization (and we may in the future), but for
		// now we're focused on supporting the primary Docker Compose use case
		// and avoiding the confusing and error-prone cases described above.
		alphaIsVolume := isVolumeURL(session.Alpha)
		betaIsVolume := isVolumeURL(session.Beta)
		if !(alphaIsVolume || betaIsVolume) {
			return fmt.Errorf("neither alpha nor beta references a volume in synchronization session (%s)", name)
		} else if alphaIsVolume && betaIsVolume {
			return fmt.Errorf("both alpha and beta reference volumes in synchronization session (%s)", name)
		}

		// Parse and validate the alpha URL. If it isn't a volume URL, then it
		// must be a local URL. In the case of a local URL, we treat relative
		// paths as relative to the project directory, so we have to override
		// the default URL parsing behavior in that case.
		var alphaURL *url.URL
		if alphaIsVolume {
			if a, volume, err := parseVolumeURL(session.Alpha, daemonMetadata.OSType); err != nil {
				return fmt.Errorf("unable to parse synchronization alpha URL (%s): %w", session.Alpha, err)
			} else {
				alphaURL = a
				volumeDependencies[volume] = true
			}
		} else {
			alphaURL, err = url.Parse(session.Alpha, url.Kind_Synchronization, true)
			if err != nil {
				return fmt.Errorf("unable to parse synchronization alpha URL (%s): %w", session.Alpha, err)
			} else if alphaURL.Protocol != url.Protocol_Local {
				return errors.New("only local and volume URLs allowed as synchronization URLs")
			}
			if !filepath.IsAbs(session.Alpha) {
				if alphaURL.Path, err = filepath.Abs(filepath.Join(project.WorkingDir, session.Alpha)); err != nil {
					return fmt.Errorf("unable to resolve relative alpha URL (%s): %w", session.Alpha, err)
				}
			}
		}

		// Parse and validate the beta URL using the same strategy.
		var betaURL *url.URL
		if betaIsVolume {
			if b, volume, err := parseVolumeURL(session.Beta, daemonMetadata.OSType); err != nil {
				return fmt.Errorf("unable to parse synchronization beta URL (%s): %w", session.Beta, err)
			} else {
				betaURL = b
				volumeDependencies[volume] = true
			}
		} else {
			betaURL, err = url.Parse(session.Beta, url.Kind_Synchronization, false)
			if err != nil {
				return fmt.Errorf("unable to parse synchronization beta URL (%s): %w", session.Beta, err)
			} else if betaURL.Protocol != url.Protocol_Local {
				return errors.New("only local and volume URLs allowed as synchronization URLs")
			}
			if !filepath.IsAbs(session.Beta) {
				if betaURL.Path, err = filepath.Abs(filepath.Join(project.WorkingDir, session.Beta)); err != nil {
					return fmt.Errorf("unable to resolve relative beta URL (%s): %w", session.Beta, err)
				}
			}
		}

		// Compute the session configuration.
		configuration := session.Configuration.Configuration()
		if err := configuration.EnsureValid(false); err != nil {
			return fmt.Errorf("invalid synchronization session configuration for %s: %v", name, err)
		}
		configuration = synchronization.MergeConfigurations(defaultConfigurationSynchronization, configuration)

		// Compute the alpha-specific configuration.
		alphaConfiguration := session.ConfigurationAlpha.Configuration()
		if err := alphaConfiguration.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid synchronization session alpha configuration for %s: %v", name, err)
		}
		alphaConfiguration = synchronization.MergeConfigurations(defaultConfigurationAlpha, alphaConfiguration)

		// Compute the beta-specific configuration.
		betaConfiguration := session.ConfigurationBeta.Configuration()
		if err := betaConfiguration.EnsureValid(true); err != nil {
			return fmt.Errorf("invalid synchronization session beta configuration for %s: %v", name, err)
		}
		betaConfiguration = synchronization.MergeConfigurations(defaultConfigurationBeta, betaConfiguration)

		// Record the specification.
		synchronizationSpecifications[name] = &synchronizationsvc.CreationSpecification{
			Alpha:              alphaURL,
			Beta:               betaURL,
			Configuration:      configuration,
			ConfigurationAlpha: alphaConfiguration,
			ConfigurationBeta:  betaConfiguration,
			Name:               name,
		}
	}

	// Validate network and volume dependencies.
	for network := range networkDependencies {
		if _, ok := project.Networks[network]; !ok {
			return fmt.Errorf("undefined network (%s) referenced by forwarding session", network)
		}
	}
	for volume := range volumeDependencies {
		if _, ok := project.Volumes[volume]; !ok {
			return fmt.Errorf("undefined volume (%s) referenced by synchronization session", volume)
		}
	}

	// Determine which services have a dependency on the volumes that Mutagen
	// is targeting and add a Mutagen service dependency for each.
	for _, service := range project.Services {
		for _, volume := range service.Volumes {
			if volume.Type == "volume" && volumeDependencies[volume.Source] {
				if service.DependsOn == nil {
					service.DependsOn = make(types.DependsOnConfig)
				}
				service.DependsOn[sidecarServiceName] = types.ServiceDependency{
					Condition: types.ServiceConditionStarted,
				}
				break
			}
		}
	}

	// Convert volume dependencies to the Compose format.
	serviceVolumeDependencies := make([]types.ServiceVolumeConfig, 0, len(volumeDependencies))
	for volume := range volumeDependencies {
		serviceVolumeDependencies = append(serviceVolumeDependencies, types.ServiceVolumeConfig{
			Type:   "volume",
			Source: volume,
			Target: mountPathForVolumeInMutagenContainer(daemonMetadata.OSType, volume),
		})
	}

	// Add the Mutagen service definition.
	project.Services = append(project.Services, types.ServiceConfig{
		Name:  sidecarServiceName,
		Image: sidecarImage,
		Labels: types.Labels{
			sidecarRoleLabelKey:    sidecarRoleLabelValue,
			sidecarVersionLabelKey: mutagen.Version,
		},
		Networks: networkDependencies,
		Volumes:  serviceVolumeDependencies,
		// TODO: Set sidecar context environment variable.
	})

	// Store session specifications.
	l.forwarding = forwardingSpecifications
	l.synchronization = synchronizationSpecifications

	// Success.
	return nil
}

// reconcileSessions performs Mutagen session reconciliation for the project
// using the specified sidecar container ID as the target identifier. It also
// ensures that all sessions are unpaused.
func (l *Liaison) reconcileSessions(sidecarID string) error {
	// Convert sidecar URLs to concrete Docker URLs.
	dockerHost := l.dockerCLI.Client().DaemonHost()
	for _, specification := range l.forwarding {
		reifySidecarURLIfNecessary(specification.Source, dockerHost, sidecarID)
		reifySidecarURLIfNecessary(specification.Destination, dockerHost, sidecarID)
		// TODO: Attach sidecar label.
	}
	for _, specification := range l.synchronization {
		reifySidecarURLIfNecessary(specification.Alpha, dockerHost, sidecarID)
		reifySidecarURLIfNecessary(specification.Beta, dockerHost, sidecarID)
		// TODO: Attach sidecar label.
	}

	// TODO: Implement.
	fmt.Println("Reconciling Mutagen sessions for", sidecarID)
	return nil
}

// listSessions lists Mutagen sessions for the project using the specified
// sidecar container ID as the target identifier.
func (l *Liaison) listSessions(sidecarID string) error {
	// TODO: Implement.
	fmt.Println("Listing Mutagen sessions for", sidecarID)
	return nil
}

// pauseSessions pauses Mutagen sessions for the project using the specified
// sidecar container ID as the target identifier.
func (l *Liaison) pauseSessions(sidecarID string) error {
	// TODO: Implement.
	fmt.Println("Pausing Mutagen sessions for", sidecarID)
	return nil
}

// resumeSessions resumes Mutagen sessions for the project using the specified
// sidecar container ID as the target identifier.
func (l *Liaison) resumeSessions(sidecarID string) error {
	// TODO: Implement.
	fmt.Println("Resuming Mutagen sessions for", sidecarID)
	return nil
}

// terminateSessions terminates Mutagen sessions for the project using the
// specified sidecar container ID as the target identifier.
func (l *Liaison) terminateSessions(sidecarID string) error {
	// TODO: Implement.
	fmt.Println("Terminating Mutagen sessions for", sidecarID)
	return nil
}