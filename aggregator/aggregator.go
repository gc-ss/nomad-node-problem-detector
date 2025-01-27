/*
Copyright 2021 Roblox Corporation

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

package aggregator

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hashicorp/nomad/api"
	types "github.com/nomad-node-problem-detector/types"
	"github.com/urfave/cli/v2"
)

var AggregatorCommand = &cli.Command{
	Name:  "aggregator",
	Usage: "Run npd in aggregator mode",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "aggregation-cycle-time",
			Aliases: []string{"t"},
			Value:   "15s",
			Usage:   "Time (in seconds) to wait between each aggregation cycle",
		},
		&cli.StringFlag{
			Name:    "detector-port",
			Aliases: []string{"p"},
			Value:   ":8083",
			Usage:   "Detector HTTP server port",
		},
		&cli.StringFlag{
			Name:    "nomad-server",
			Aliases: []string{"s"},
			Value:   "http://localhost:4646",
			Usage:   "HTTP API address of a Nomad server or agent.",
		},
	},
	Action: func(c *cli.Context) error {
		return aggregate(c)
	},
}

var pause bool

func aggregate(context *cli.Context) error {
	nomadServer := context.String("nomad-server")
	client, err := getNomadClient(nomadServer)
	if err != nil {
		return err
	}

	aggregationCycleTime, err := time.ParseDuration(context.String("aggregation-cycle-time"))
	if err != nil {
		return err
	}

	detectorPort := context.String("detector-port")

	authToken := os.Getenv("DETECTOR_HTTP_TOKEN")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1)
	go flipPause(sigs)

	nodeHandle := client.Nodes()

	queryOptions := &api.QueryOptions{AllowStale: true}

	// map[nodeID][node health check /v1/nodehealth/]
	m := make(map[string][]types.HealthCheck)
	for {
		if pause {
			// Aggregator is paused. Wait for unpause.
			continue
		}

		log.Info("Collect and aggregate nodes health")
		nodes, _, err := nodeHandle.List(queryOptions)
		if err != nil {
			errMsg := fmt.Sprintf("Error in listing nomad nodes: %v\n", err)
			log.Warning(errMsg)
			time.Sleep(aggregationCycleTime)
			continue
		}

		for _, node := range nodes {
			npdServer := fmt.Sprintf("http://%s%s", node.Address, detectorPort)

			npdActive, err := isNpdServerActive(npdServer, authToken)
			if err != nil {
				errMsg := fmt.Sprintf("Node %s is unreachable, skipping node.", node.Address)
				log.Warning(errMsg)
				continue
			}

			if !npdActive {
				errMsg := fmt.Sprintf("Node %s is unhealthy, marking it as ineligible.", node.Address)
				log.Warning(errMsg)
				toggleNodeEligibility(nodeHandle, node.ID, node.Address, false)
				continue
			}

			url := npdServer + "/v1/nodehealth/"
			req, err := http.NewRequest("POST", url, nil)
			if err != nil {
				errMsg := fmt.Sprintf("Error in building /v1/nodehealth/ HTTP request, skipping node %s\n", node.Address)
				log.Warning(errMsg)
				continue
			}

			if authToken != "" {
				base64EncodedToken := base64.StdEncoding.EncodeToString([]byte(authToken))
				req.Header.Set("Authorization", "Basic "+base64EncodedToken)
			}

			req.Header.Set("Content-Type", "application/json")
			client := &http.Client{Timeout: time.Second * 5}
			resp, err := client.Do(req)
			if err != nil {
				errMsg := fmt.Sprintf("Error in getting /v1/nodehealth/ HTTP response, skipping node %s\n", node.Address)
				log.Warning(errMsg)
				resp.Body.Close()
				continue
			}

			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				errMsg := fmt.Sprintf("Error in reading /v1/nodehealth/ HTTP response, skipping node %s\n", node.Address)
				log.Warning(errMsg)
				resp.Body.Close()
				continue
			}

			resp.Body.Close()

			current := []types.HealthCheck{}
			if err := json.Unmarshal(body, &current); err != nil {
				errMsg := fmt.Sprintf("Error in unmarshalling /v1/nodehealth/ HTTP response body, skipping node %s\n", node.Address)
				log.Warning(errMsg)
				continue
			}

			var nodeHealth []types.HealthCheck
			if m[node.ID] != nil {
				nodeHealth = m[node.ID]
			}

			previous := make(map[string]types.HealthCheck)
			for _, nh := range nodeHealth {
				previous[nh.Type] = nh
			}

			nodeHealthy := true
			stateChanged := false

			for _, curr := range current {
				// Default CPU, memory and disk checks are represented with
				// boolean (true/false). curr.Result = true for CPUUnderPressure
				// or MemoryUnderPressure or DiskUsageHigh tells that the system
				// is under CPU/memory/disk pressure and should be taken out of
				// eligibility.
				if curr.Result == "Unhealthy" || curr.Result == "true" {
					errMsg := fmt.Sprintf("Node %s: %s is %s\n", node.Address, curr.Type, curr.Result)
					log.Warning(errMsg)
					nodeHealthy = false
				}

				prev, ok := previous[curr.Type]
				if ok {
					if prev.Result == curr.Result {
						continue
					} else {
						stateChanged = true
					}
				}
			}

			if len(previous) == 0 || stateChanged {
				if nodeHealthy {
					toggleNodeEligibility(nodeHandle, node.ID, node.Address, true)
				} else {
					toggleNodeEligibility(nodeHandle, node.ID, node.Address, false)
				}
			}
			m[node.ID] = current
		}
		time.Sleep(aggregationCycleTime)
	}
	return nil
}

// Toggle Nomad node eligibility.
func toggleNodeEligibility(nodeHandle *api.Nodes, nodeID, nodeAddress string, eligible bool) {
	if _, err := nodeHandle.ToggleEligibility(nodeID, eligible, nil); err != nil {
		errMsg := fmt.Sprintf("Error in toggling node eligibility, skipping node %s\n", nodeAddress)
		log.Warning(errMsg)
	}
}

// Check if Nomad node problem detector (nNPD) HTTP server is healthy and active.
func isNpdServerActive(npdServer, authToken string) (bool, error) {
	url := npdServer + "/v1/health/"
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return false, err
	}

	if authToken != "" {
		base64EncodedToken := base64.StdEncoding.EncodeToString([]byte(authToken))
		req.Header.Set("Authorization", "Basic "+base64EncodedToken)
	}

	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: time.Second * 5}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false, nil
	}
	return true, nil
}

// flipPause pauses and unpauses aggregator based on receiving SIGUSR1 signal.
func flipPause(sigs chan os.Signal) {
	for {
		select {
		case <-sigs:
			pause = !pause
			if pause {
				log.Info("Received signal SIGUSR1, pausing aggregator.")
			} else {
				log.Info("Received signal SIGUSR1, unpausing aggregator.")
			}
		}
	}
}

// Get Nomad HTTP client.
// This client will be used to list nodes and toggle node eligibility.
func getNomadClient(nomadServer string) (*api.Client, error) {
	cfg := api.DefaultConfig()
	cfg.Address = nomadServer
	cfg.TLSConfig.Insecure = true

	timeout, err := time.ParseDuration("5s")
	if err != nil {
		return nil, err
	}

	cfg.HttpClient = &http.Client{Timeout: timeout}
	client, err := api.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return client, nil
}
