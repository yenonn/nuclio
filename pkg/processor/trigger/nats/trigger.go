/*
Copyright 2017 The Nuclio Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nats

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/nuclio/nuclio/pkg/common"
	"github.com/nuclio/nuclio/pkg/functionconfig"
	"github.com/nuclio/nuclio/pkg/processor/trigger"
	"github.com/nuclio/nuclio/pkg/processor/worker"

	natsio "github.com/nats-io/nats.go"
	"github.com/nuclio/errors"
	"github.com/nuclio/logger"
)

type nats struct {
	trigger.AbstractTrigger
	event            Event
	configuration    *Configuration
	stop             chan bool
	natsSubscription *natsio.Subscription
}

func newTrigger(parentLogger logger.Logger,
	workerAllocator worker.Allocator,
	configuration *Configuration,
	restartTriggerChan chan trigger.Trigger) (trigger.Trigger, error) {
	abstractTrigger, err := trigger.NewAbstractTrigger(parentLogger.GetChild(configuration.ID),
		workerAllocator,
		&configuration.Configuration,
		"async",
		"nats",
		configuration.Name,
		restartTriggerChan)
	if err != nil {
		return nil, errors.New("Failed to create abstract trigger")
	}

	newTrigger := &nats{
		AbstractTrigger: abstractTrigger,
		configuration:   configuration,
		stop:            make(chan bool),
	}
	newTrigger.AbstractTrigger.Trigger = newTrigger

	err = newTrigger.validateConfiguration()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to validate NATS trigger configuration")
	}

	return newTrigger, nil
}

func (n *nats) validateConfiguration() error {
	natsURL, err := url.Parse(n.configuration.URL)
	if err != nil {
		return errors.Wrap(err, "Failed to parse NATS URL")
	}

	if natsURL.Scheme != "nats" {
		return errors.New("Invalid URL. Must begin with 'nats://'")
	}

	return nil
}

func (n *nats) Start(checkpoint functionconfig.Checkpoint) error {
	queueName := n.configuration.QueueName
	if queueName == "" {
		queueName = "{{.Namespace}}.{{.Name}}-{{.Id}}"
	}

	queueNameTemplate, err := template.New("queueName").Parse(queueName)
	if err != nil {
		return errors.Wrap(err, "Failed to create queueName template")
	}

	var queueNameTemplateBuffer bytes.Buffer
	err = queueNameTemplate.Execute(&queueNameTemplateBuffer, &map[string]interface{}{
		"Namespace":   n.configuration.RuntimeConfiguration.Meta.Namespace,
		"Name":        n.configuration.RuntimeConfiguration.Meta.Name,
		"Id":          n.configuration.ID,
		"Labels":      n.configuration.RuntimeConfiguration.Meta.Labels,
		"Annotations": n.configuration.RuntimeConfiguration.Meta.Annotations,
	})
	if err != nil {
		return errors.Wrap(err, "Failed to execute queueName template")
	}
	// source rootCACert for TLS connection.
	rootCACerts := os.Getenv("NUCLIO_ROOT_CA_CERTS_FILE")
	if _, err := os.Stat(rootCACerts); err != nil {
		return errors.Wrapf(err, "Root CA Certificate is missing.")
	}

	// source auth credential for connection.
	auth_username, err := os.ReadFile("/nats-secret/username")
	if err != nil {
		return errors.Wrapf(err, "Cannot read nats username.")
	}

	auth_password, err := os.ReadFile("/nats-secret/password")
	if err != nil {
		return errors.Wrapf(err, "Cannot read nats credential.")
	}

	queueName = queueNameTemplateBuffer.String()
	n.Logger.InfoWith("Starting",
		"serverURL", n.configuration.URL,
		"rootCACert", rootCACerts,
		"topic", n.configuration.Topic,
		"queueName", queueName)

	natsConnection, err := natsio.Connect(
		n.configuration.URL,
		natsio.RootCAs(rootCACerts),
		natsio.UserInfo(string(auth_username), string(auth_password)),
	)
	if err != nil {
		return errors.Wrapf(err, "Can't connect to NATS server %s", n.configuration.URL)
	}

	natsJsConnection, err := natsConnection.JetStream()
	if err != nil {
		return errors.Wrapf(err, "Can't connect to NATS JetStream server %s", n.configuration.URL)
	}

	if info, err := natsJsConnection.StreamInfo(n.configuration.QueueName); err != nil {
		// not found any info, create it now
		streamWildCard := fmt.Sprintf("%s.>", n.configuration.QueueName)
		streamConfig := natsio.StreamConfig{Name: n.configuration.QueueName,
			Subjects:  []string{streamWildCard},
			Retention: natsio.LimitsPolicy,
			MaxAge:    7 * 24 * time.Hour}
		_, err := natsJsConnection.AddStream(&streamConfig)
		if err != nil {
			return errors.Wrapf(err, "Can't create the Jetstream stream %s", n.configuration.QueueName)
		}
		n.Logger.InfoWith("Jetstream created", "stream", info.Config.Name)
	} else {
		n.Logger.InfoWith("Jetstream", "stream", info.Config.Name)
	}
	durableName := strings.Replace(n.configuration.Topic, ".", "-", -1)
	messageChan := make(chan *natsio.Msg, 64)
	if _, err := natsJsConnection.ConsumerInfo(n.configuration.QueueName, n.configuration.Topic); err != nil {
		n.Logger.InfoWith("Consumer created", "consumer", durableName)
		n.natsSubscription, err = natsJsConnection.ChanQueueSubscribe(n.configuration.Topic, n.configuration.QueueName, messageChan, natsio.Durable(durableName))
		if err != nil {
			return errors.Wrapf(err, "Can't subscribe to topic %q in queue %q", n.configuration.Topic, queueName)
		}
	} else {
		n.Logger.InfoWith("Consumer", "consumer", durableName)
	}
	go n.listenForMessages(messageChan)
	return nil
}

func (n *nats) Stop(force bool) (functionconfig.Checkpoint, error) {
	n.stop <- true
	return nil, n.natsSubscription.Unsubscribe()
}

func (n *nats) listenForMessages(messageChan chan *natsio.Msg) {
	for {
		select {
		case natsMessage := <-messageChan:
			if n.WorkerAllocator.GetNumWorkersAvailable() == 0 {
				err := natsMessage.NakWithDelay(10 * time.Second)
				if err != nil {
					n.Logger.ErrorWith("Can't ack message", "error", err)
				}
			} else {
				go func() {
					err := natsMessage.AckSync()
					if err != nil {
						n.Logger.ErrorWith("Can't ack message", "error", err)
					}
					n.event.natsMessage = natsMessage
					// process the event, don't really do anything with response
					_, submitError, processError := n.AllocateWorkerAndSubmitEvent(&n.event, n.Logger, 10*time.Second)
					if submitError != nil {
						n.Logger.ErrorWith("Can't submit event", "error", submitError)
					}
					if processError != nil {
						n.Logger.ErrorWith("Can't process event", "error", processError)
					}
				}()
			}
		case <-n.stop:
			return
		}
	}
}

func (n *nats) GetConfig() map[string]interface{} {
	return common.StructureToMap(n.configuration)
}
