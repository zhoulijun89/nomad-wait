package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

const (
	STATUS_HEALTHY   = "healthy"
	STATUS_UNHEALTHY = "unhealthy"
	STATUS_COMPLETE  = "complete"
	STATUS_FAILED    = "failed"
	STATUS_RUNNING   = "running"
)

type Allocation struct {
	api.AllocationListStub
	Index int
}

func findAllocation(allocs []*Allocation, taskGroup string, index int) *Allocation {
	for _, alloc := range allocs {
		if alloc.TaskGroup == taskGroup && alloc.Index == index {
			return alloc
		}
	}
	return nil
}

// Retrieves the value of the environment variable named by the `key`
// It returns the value if variable present and value not empty
// Otherwise it returns string value `def`
func stringFromEnv(key string, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// Retrieves the value of the environment variable named by the `key`
// It returns the value if variable present and value not empty
// Otherwise it returns string value `def`
func intFromEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			panic(fmt.Sprintf("Error read environment variable: %v", err))
		}
		return i
	}
	return def
}

func jobNameFromArgs(args []string) (string, error) {
	if len(args) == 0 || len(args) > 1 {
		return "", errors.New("incorrect number of arguments")
	}
	return args[0], nil
}

func parseAllocationName(name string) (string, string, int, error) {
	nameRegexp := regexp.MustCompile(`([\w-]+)\.([\w-]+)\[(\d+)\]`)
	match := nameRegexp.FindStringSubmatch(name)
	if len(match) == 0 {
		return "", "", -1, fmt.Errorf("could not parse allocation name %s", name)
	}

	index, err := strconv.Atoi(match[3])
	if err != nil {
		return "", "", -1, fmt.Errorf("could not parse allocation name %s: %v", name, err)
	}

	return match[1], match[2], index, nil
}

func monitoredAllocations(client *api.Client, jobName string) ([]*Allocation, error) {
	var result []*Allocation
	q := api.QueryOptions{}

	allocs, _, err := client.Allocations().List(&q)
	if err != nil {
		return nil, fmt.Errorf("unable get allocations list from Nomad server: %v", err)
	}

	for _, a := range allocs {
		if a.JobID != jobName {
			continue
		}

		_, _, index, err := parseAllocationName(a.Name)
		if err != nil {
			return nil, err
		}

		if alloc := findAllocation(result, a.TaskGroup, index); alloc != nil {
			if alloc.CreateTime < a.CreateTime {
				alloc = &Allocation{*a, index}
			}
		} else {
			result = append(result, &Allocation{*a, index})
		}
	}

	return result, nil
}

func allocationStatus(alloc *Allocation) string {
	human_readable := map[bool]string{true: STATUS_HEALTHY, false: STATUS_UNHEALTHY}

	if alloc.ClientStatus == STATUS_RUNNING && alloc.DeploymentStatus != nil {
		return human_readable[*alloc.DeploymentStatus.Healthy]
	}

	return alloc.ClientStatus
}

func Run() int {
	var timeout int
	var acceptableStatus string
	addressHelpText := `
The address of the Nomad server.
Overrides the NOMAD_ADDR environment variable if set.
Default = http://127.0.0.1:4646 
	`
	timeoutHelpText := `
Wait timeout in seconds
Default = 60	
	`
	nomadConfig := api.DefaultConfig()

	flag.StringVar(&nomadConfig.Address, "address", stringFromEnv("NOMAD_ADDR", "http://127.0.0.1:4646"), strings.TrimSpace(addressHelpText))
	flag.IntVar(&timeout, "timeout", intFromEnv("NOMAD_JOB_TIMEOUT", 60), strings.TrimSpace(timeoutHelpText))
	flag.IntVar(&timeout, "t", intFromEnv("NOMAD_JOB_TIMEOUT", 60), strings.TrimSpace(timeoutHelpText))
	flag.Parse()

	jobName, err := jobNameFromArgs(flag.Args())
	if err != nil {
		fmt.Println(err)
		fmt.Printf("Usage: %s [args] <name>", os.Args[0])
		flag.PrintDefaults()
		return 1
	}

	client, err := api.NewClient(nomadConfig)
	if err != nil {
		fmt.Print(err)
		return 1
	}

	fmt.Printf("Wait Nomad job %s for %ds\n", jobName, timeout)

	for i := 0; i < timeout; i++ {
		var indicator string
		successful := true
		failed := false

		allocs, err := monitoredAllocations(client, jobName)
		if err != nil {
			fmt.Print(err)
			return 1
		}

		if len(allocs) == 0 {
			fmt.Printf("Allocations for %s not found", jobName)
			return 1
		}

		if allocs[0].JobType == "batch" {
			acceptableStatus = STATUS_COMPLETE
		} else {
			acceptableStatus = STATUS_HEALTHY
		}

		for _, a := range allocs {
			status := allocationStatus(a)

			if status == STATUS_FAILED {
				failed = true
				indicator += "!"
				continue
			}

			if status == acceptableStatus {
				indicator += "+"
				continue
			}

			indicator += "-"
			successful = false
		}

		if failed {
			fmt.Printf("[%s] Allocation failed\n", indicator)
			return 1
		}

		if successful {
			fmt.Printf("[%s] Allocation successful\n", indicator)
			return 0
		}

		if x := i % 10; x == 0 {
			fmt.Printf("[%s] Allocation in progress for %ds\n", indicator, i)
		}

		time.Sleep(time.Second)
	}

	fmt.Print("Allocation timed out\n")
	return 1
}

func main() {
	os.Exit(Run())
}
