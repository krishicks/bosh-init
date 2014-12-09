package instance

import (
	"fmt"
	"time"

	bosherr "github.com/cloudfoundry/bosh-agent/errors"
	boshlog "github.com/cloudfoundry/bosh-agent/logger"

	bmcloud "github.com/cloudfoundry/bosh-micro-cli/cloud"
	bmregistry "github.com/cloudfoundry/bosh-micro-cli/deployer/registry"
	bmsshtunnel "github.com/cloudfoundry/bosh-micro-cli/deployer/sshtunnel"
	bmstemcell "github.com/cloudfoundry/bosh-micro-cli/deployer/stemcell"
	bmvm "github.com/cloudfoundry/bosh-micro-cli/deployer/vm"
	bmdepl "github.com/cloudfoundry/bosh-micro-cli/deployment"
	bmeventlog "github.com/cloudfoundry/bosh-micro-cli/eventlogger"
)

type Manager interface {
	FindCurrent() ([]Instance, error)
	Create(
		jobName string,
		id int,
		deployment bmdepl.Deployment,
		extractedStemcell bmstemcell.ExtractedStemcell,
		cloudStemcell bmstemcell.CloudStemcell,
		registry bmdepl.Registry,
		sshTunnelConfig bmdepl.SSHTunnel,
		eventLoggerStage bmeventlog.Stage,
	) (instance Instance, err error)
	DeleteAll(
		pingTimeout time.Duration,
		pingDelay time.Duration,
		eventLoggerStage bmeventlog.Stage,
	) error
}

type manager struct {
	cloud            bmcloud.Cloud
	vmManager        bmvm.Manager
	registryServer   bmregistry.Server
	sshTunnelFactory bmsshtunnel.Factory
	diskDeployer     DiskDeployer
	logger           boshlog.Logger
	logTag           string
}

func NewManager(
	cloud bmcloud.Cloud,
	vmManager bmvm.Manager,
	registryServer bmregistry.Server,
	sshTunnelFactory bmsshtunnel.Factory,
	diskDeployer DiskDeployer,
	logger boshlog.Logger,
) Manager {
	return &manager{
		cloud:            cloud,
		vmManager:        vmManager,
		registryServer:   registryServer,
		sshTunnelFactory: sshTunnelFactory,
		diskDeployer:     diskDeployer,
		logger:           logger,
		logTag:           "vmDeployer",
	}
}

func (m *manager) FindCurrent() ([]Instance, error) {
	instances := []Instance{}

	// Only one current instance will exist (for now)
	vm, found, err := m.vmManager.FindCurrent()
	if err != nil {
		return instances, bosherr.WrapError(err, "Finding currently deployed instances")
	}

	if !found {
		return instances, nil
	}

	// the job name is not stored (yet)
	instance := NewInstance("unknown", 0, vm, m.vmManager, m.sshTunnelFactory, m.logger)
	instances = append(instances, instance)

	return instances, err
}

func (m *manager) Create(
	jobName string,
	id int,
	deployment bmdepl.Deployment,
	extractedStemcell bmstemcell.ExtractedStemcell,
	cloudStemcell bmstemcell.CloudStemcell,
	registry bmdepl.Registry,
	sshTunnelConfig bmdepl.SSHTunnel,
	eventLoggerStage bmeventlog.Stage,
) (instance Instance, err error) {

	if !registry.IsEmpty() {
		registryReadyErrCh := make(chan error)
		go m.startRegistry(registry, registryReadyErrCh)
		defer m.registryServer.Stop()

		err = <-registryReadyErrCh
		if err != nil {
			return instance, bosherr.WrapError(err, "Starting registry")
		}
	}

	var vm bmvm.VM
	stepName := fmt.Sprintf("Creating VM for instance '%s/%d' from stemcell '%s'", jobName, id, cloudStemcell.CID())
	err = eventLoggerStage.PerformStep(stepName, func() error {
		vm, err = m.vmManager.Create(cloudStemcell, deployment)
		if err != nil {
			return bosherr.WrapError(err, "Creating VM")
		}

		if err = cloudStemcell.PromoteAsCurrent(); err != nil {
			return bosherr.WrapErrorf(err, "Promoting stemcell as current '%s'", cloudStemcell.CID())
		}

		return nil
	})
	if err != nil {
		return instance, err
	}

	instance = NewInstance(jobName, id, vm, m.vmManager, m.sshTunnelFactory, m.logger)

	sshTunnelOptions := bmsshtunnel.Options{
		Host:              sshTunnelConfig.Host,
		Port:              sshTunnelConfig.Port,
		User:              sshTunnelConfig.User,
		Password:          sshTunnelConfig.Password,
		PrivateKey:        sshTunnelConfig.PrivateKey,
		LocalForwardPort:  registry.Port,
		RemoteForwardPort: registry.Port,
	}

	if err := instance.WaitUntilReady(sshTunnelOptions, eventLoggerStage); err != nil {
		return instance, bosherr.WrapError(err, "Waiting until instance is ready")
	}

	// disk creation requires knowledge of the vm, so we can't use the diskManager.Create pattern
	diskPool, err := deployment.DiskPool(jobName)
	if err != nil {
		return instance, bosherr.WrapError(err, "Getting disk pool")
	}

	err = m.diskDeployer.Deploy(diskPool, m.cloud, vm, eventLoggerStage)
	if err != nil {
		return instance, bosherr.WrapError(err, "Deploying disk")
	}

	if err = instance.StartJobs(extractedStemcell.ApplySpec(), deployment, eventLoggerStage); err != nil {
		return instance, err
	}

	return instance, err
}

func (m *manager) DeleteAll(
	pingTimeout time.Duration,
	pingDelay time.Duration,
	eventLoggerStage bmeventlog.Stage,
) error {
	instances, err := m.FindCurrent()
	if err != nil {
		return err
	}

	for _, instance := range instances {
		if err = instance.Delete(pingTimeout, pingDelay, eventLoggerStage); err != nil {
			return bosherr.WrapErrorf(err, "Deleting existing instance '%s/%d'", instance.JobName(), instance.ID())
		}
	}
	return nil
}

func (m *manager) startRegistry(registry bmdepl.Registry, readyErrCh chan error) {
	err := m.registryServer.Start(registry.Username, registry.Password, registry.Host, registry.Port, readyErrCh)
	if err != nil {
		m.logger.Debug(m.logTag, "Registry error occurred: %s", err.Error())
	}
}