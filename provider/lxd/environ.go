// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

// +build go1.3

package lxd

import (
	"sync"

	"github.com/juju/errors"

	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/config"
	"github.com/juju/juju/environs/tags"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/tools/lxdclient"
)

const bootstrapMessage = `To configure your system to better support LXD containers, please see: https://github.com/lxc/lxd/blob/master/doc/production-setup.md`

type baseProvider interface {
	// BootstrapEnv bootstraps a Juju environment.
	BootstrapEnv(environs.BootstrapContext, environs.BootstrapParams) (*environs.BootstrapResult, error)

	// DestroyEnv destroys the provided Juju environment.
	DestroyEnv() error
}

type environ struct {
	cloud    environs.CloudSpec
	provider *environProvider

	// local records whether or not the LXD is local to the host running
	// this process.
	local bool

	name string
	uuid string
	raw  *rawProvider
	base baseProvider

	// namespace is used to create the machine and device hostnames.
	namespace instance.Namespace

	lock sync.Mutex
	ecfg *environConfig
}

type newRawProviderFunc func(environs.CloudSpec, bool) (*rawProvider, error)

func newEnviron(
	provider *environProvider,
	local bool,
	spec environs.CloudSpec,
	cfg *config.Config,
	newRawProvider newRawProviderFunc,
) (*environ, error) {
	ecfg, err := newValidConfig(cfg)
	if err != nil {
		return nil, errors.Annotate(err, "invalid config")
	}

	namespace, err := instance.NewNamespace(cfg.UUID())
	if err != nil {
		return nil, errors.Trace(err)
	}

	raw, err := newRawProvider(spec, local)
	if err != nil {
		return nil, errors.Trace(err)
	}

	env := &environ{
		cloud:     spec,
		local:     local,
		name:      ecfg.Name(),
		uuid:      ecfg.UUID(),
		raw:       raw,
		namespace: namespace,
		ecfg:      ecfg,
	}
	env.base = common.DefaultProvider{Env: env}

	//TODO(wwitzel3) make sure we are also cleaning up profiles during destroy
	if err := env.initProfile(); err != nil {
		return nil, errors.Trace(err)
	}

	return env, nil
}

var defaultProfileConfig = map[string]string{
	"boot.autostart":   "true",
	"security.nesting": "true",
}

func (env *environ) initProfile() error {
	hasProfile, err := env.raw.HasProfile(env.profileName())
	if err != nil {
		return errors.Trace(err)
	}

	if hasProfile {
		return nil
	}

	return env.raw.CreateProfile(env.profileName(), defaultProfileConfig)
}

func (env *environ) profileName() string {
	return "juju-" + env.ecfg.Name()
}

// Name returns the name of the environment.
func (env *environ) Name() string {
	return env.name
}

// Provider returns the environment provider that created this env.
func (env *environ) Provider() environs.EnvironProvider {
	return env.provider
}

// SetConfig updates the env's configuration.
func (env *environ) SetConfig(cfg *config.Config) error {
	env.lock.Lock()
	defer env.lock.Unlock()
	ecfg, err := newValidConfig(cfg)
	if err != nil {
		return errors.Trace(err)
	}
	env.ecfg = ecfg
	return nil
}

// Config returns the configuration data with which the env was created.
func (env *environ) Config() *config.Config {
	env.lock.Lock()
	cfg := env.ecfg.Config
	env.lock.Unlock()
	return cfg
}

// PrepareForBootstrap implements environs.Environ.
func (env *environ) PrepareForBootstrap(ctx environs.BootstrapContext) error {
	if err := lxdclient.EnableHTTPSListener(env.raw); err != nil {
		return errors.Annotate(err, "enabling HTTPS listener")
	}
	return nil
}

// Create implements environs.Environ.
func (env *environ) Create(environs.CreateParams) error {
	return nil
}

// Bootstrap implements environs.Environ.
func (env *environ) Bootstrap(ctx environs.BootstrapContext, params environs.BootstrapParams) (*environs.BootstrapResult, error) {
	if env.local {
		// Add the client certificate to the LXD server, so the
		// controller containers can authenticate. We can only
		// do this for local LXD. For non-local, the user must
		// do this themselves, until we support using trust
		// passwords.
		clientCert, _, ok := getCerts(env.cloud)
		if !ok {
			return nil, errors.New("cannot bootstrap without client certificate")
		}
		fingerprint, err := clientCert.Fingerprint()
		if err != nil {
			return nil, errors.Trace(err)
		}
		_, err = env.raw.CertByFingerprint(fingerprint)
		if errors.IsNotFound(err) {
			if err := env.raw.AddCert(*clientCert); err != nil {
				return nil, errors.Annotatef(
					err, "adding certificate %q", clientCert.Name,
				)
			}
		} else if err != nil {
			return nil, errors.Annotate(err, "querying certificates")
		}
	}
	return env.base.BootstrapEnv(ctx, params)
}

// BootstrapMessage is part of the Environ interface.
func (env *environ) BootstrapMessage() string {
	return bootstrapMessage
}

// Destroy shuts down all known machines and destroys the rest of the
// known environment.
func (env *environ) Destroy() error {
	rules, err := env.IngressRules()
	if err != nil {
		return errors.Trace(err)
	}
	if len(rules) > 0 {
		if err := env.ClosePorts(rules); err != nil {
			return errors.Trace(err)
		}
	}
	if err := env.base.DestroyEnv(); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// DestroyController implements the Environ interface.
func (env *environ) DestroyController(controllerUUID string) error {
	if err := env.Destroy(); err != nil {
		return errors.Trace(err)
	}
	if err := env.destroyHostedModelResources(controllerUUID); err != nil {
		return errors.Trace(err)
	}
	if env.local {
		// When we're running locally to the LXD host, remove the
		// certificate from LXD. It will get added back in at
		// bootstrap time as necessary. For remote LXD, the user
		// needs to have added the certificate to LXD themselves.
		if err := env.removeCertificate(); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (env *environ) destroyHostedModelResources(controllerUUID string) error {
	// Destroy all instances with juju-controller-uuid
	// matching the specified UUID.
	const prefix = "juju-"
	instances, err := env.prefixedInstances(prefix)
	if err != nil {
		return errors.Annotate(err, "listing instances")
	}
	logger.Debugf("instances: %v", instances)
	var names []string
	for _, inst := range instances {
		metadata := inst.raw.Metadata()
		if metadata[tags.JujuModel] == env.uuid {
			continue
		}
		if metadata[tags.JujuController] != controllerUUID {
			continue
		}
		names = append(names, string(inst.Id()))
	}
	if len(names) > 0 {
		if err := env.raw.RemoveInstances(prefix, names...); err != nil {
			return errors.Annotate(err, "removing hosted model instances")
		}
	}
	return nil
}

func (env *environ) removeCertificate() error {
	if env.raw.remote.Cert == nil {
		return nil
	}
	fingerprint, err := env.raw.remote.Cert.Fingerprint()
	if err != nil {
		return errors.Annotate(err, "generating certificate fingerprint")
	}
	err = env.raw.RemoveCertByFingerprint(fingerprint)
	if err != nil && !errors.IsNotFound(err) {
		return errors.Annotate(err, "removing certificate")
	}
	return nil
}
