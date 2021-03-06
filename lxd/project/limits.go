package project

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/units"
	"github.com/pkg/errors"
)

// CheckLimitsUponInstanceCreation returns an error if any project-specific
// limit is violated when creating a new instance.
func CheckLimitsUponInstanceCreation(tx *db.ClusterTx, projectName string, req api.InstancesPost) error {
	project, profiles, instances, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}
	if project == nil {
		return nil
	}

	err = checkInstanceCountLimit(project, len(instances), req.Type)
	if err != nil {
		return err
	}

	// Add the instance being created.
	instances = append(instances, db.Instance{
		Profiles: req.Profiles,
		Config:   req.Config,
	})

	err = checkAggregateInstanceLimits(tx, project, instances, profiles)
	if err != nil {
		return err
	}

	return nil
}

// Check that we have not reached the maximum number of instances for
// this type.
func checkInstanceCountLimit(project *api.Project, instanceCount int, instanceType api.InstanceType) error {
	var key string
	switch instanceType {
	case api.InstanceTypeContainer:
		key = "limits.containers"
	case api.InstanceTypeVM:
		key = "limits.virtual-machines"
	default:
		return fmt.Errorf("Unexpected instance type '%s'", instanceType)
	}
	value, ok := project.Config[key]
	if ok {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 0 {
			return fmt.Errorf("Unexpected '%s' value: '%s'", key, value)
		}
		if instanceCount >= limit {
			return fmt.Errorf(
				"Reached maximum number of instances of type %s in project %s",
				instanceType, project.Name)
		}
	}

	return nil
}

// Check that we would not violate the project limits if we were to commit the
// given instances and profiles.
func checkAggregateInstanceLimits(tx *db.ClusterTx, project *api.Project, instances []db.Instance, profiles []db.Profile) error {
	// List of config keys for which we need to check aggregate values
	// across all project instances.
	aggregateKeys := []string{}
	for key := range project.Config {
		if shared.StringInSlice(key, []string{"limits.memory", "limits.processes", "limits.cpu"}) {
			aggregateKeys = append(aggregateKeys, key)
		}
	}
	if len(aggregateKeys) == 0 {
		return nil
	}

	instances = expandInstancesConfig(instances, profiles)

	totals, err := getTotalsAcrossInstances(instances, aggregateKeys)
	if err != nil {
		return err
	}

	for _, key := range aggregateKeys {
		parser := aggregateLimitConfigValueParsers[key]
		max, err := parser(project.Config[key])
		if err != nil {
			return err
		}
		if totals[key] > max {
			return fmt.Errorf(
				"Reached maximum aggregate value %s for %q in project %s",
				project.Config[key], key, project.Name)
		}
	}

	return nil
}

// CheckLimitsUponInstanceUpdate returns an error if any project-specific limit
// is violated when updating an existing instance.
func CheckLimitsUponInstanceUpdate(tx *db.ClusterTx, projectName, instanceName string, req api.InstancePut) error {
	project, profiles, instances, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}
	if project == nil {
		return nil
	}

	// Change the instance being updated.
	for i, instance := range instances {
		if instance.Name != instanceName {
			continue
		}
		instances[i].Profiles = req.Profiles
		instances[i].Config = req.Config
	}

	err = checkAggregateInstanceLimits(tx, project, instances, profiles)
	if err != nil {
		return err
	}

	return nil
}

// CheckLimitsUponProfileUpdate checks that project limits are not violated
// when changing a profile.
func CheckLimitsUponProfileUpdate(tx *db.ClusterTx, projectName, profileName string, req api.ProfilePut) error {
	project, profiles, instances, err := fetchProject(tx, projectName, true)
	if err != nil {
		return err
	}
	if project == nil {
		return nil
	}

	// Change the profile being updated.
	for i, profile := range profiles {
		if profile.Name != profileName {
			continue
		}
		profiles[i].Config = req.Config
	}

	err = checkAggregateInstanceLimits(tx, project, instances, profiles)
	if err != nil {
		return err
	}

	return nil
}

// ValidateLimitsUponProjectUpdate checks the new limits to be set on a project
// are valid.
func ValidateLimitsUponProjectUpdate(tx *db.ClusterTx, projectName string, config map[string]string, changed []string) error {
	_, profiles, instances, err := fetchProject(tx, projectName, false)
	if err != nil {
		return err
	}

	instances = expandInstancesConfig(instances, profiles)

	// List of keys that need to check aggregate values across all project
	// instances.
	aggregateKeys := []string{}

	for _, key := range changed {
		switch key {
		case "limits.containers":
			fallthrough
		case "limits.virtual-machines":
			err := validateInstanceCountLimit(instances, key, config[key], projectName)
			if err != nil {
				return err
			}
		case "limits.processes":
			fallthrough
		case "limits.cpu":
			fallthrough
		case "limits.memory":
			aggregateKeys = append(aggregateKeys, key)

		}
	}

	if len(aggregateKeys) > 0 {
		totals, err := getTotalsAcrossInstances(instances, aggregateKeys)
		if err != nil {
			return err
		}
		for _, key := range aggregateKeys {
			err := validateAggregateLimit(totals, key, config[key])
			if err != nil {
				return err
			}
		}

	}

	return nil
}

// Check that limits.containers or limits.virtual-machines is equal or above
// the current count.
func validateInstanceCountLimit(instances []db.Instance, key, value, project string) error {
	instanceType := countConfigInstanceType[key]
	limit, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	dbType, err := instancetype.New(string(instanceType))
	if err != nil {
		return err
	}
	count := 0
	for _, instance := range instances {
		if instance.Type == dbType {
			count++
		}
	}
	if limit < count {
		return fmt.Errorf(
			"'%s' is too low: there currently are %d instances of type %s in project %s",
			key, count, instanceType, project)
	}
	return nil
}

var countConfigInstanceType = map[string]api.InstanceType{
	"limits.containers":       api.InstanceTypeContainer,
	"limits.virtual-machines": api.InstanceTypeVM,
}

// Validates an aggregate limit, checking that the new value is not below the
// current total amount.
func validateAggregateLimit(totals map[string]int64, key, value string) error {
	parser := aggregateLimitConfigValueParsers[key]
	limit, err := parser(value)
	if err != nil {
		errors.Wrapf(err, "Invalid value '%s' for limit %s", value, key)
	}

	total := totals[key]
	if limit < total {
		printer := aggregateLimitConfigValuePrinters[key]
		return fmt.Errorf("'%s' is too low: current total is %s", key, printer(total))
	}

	return nil
}

// Return true if the project has some limits.
func projectHasLimits(project *api.Project) bool {
	for k := range project.Config {
		if strings.HasPrefix(k, "limits.") {
			return true
		}
	}
	return false
}

// Fetch the given project from the database along with its profiles and instances.
//
// If the skipIfNoLimits flag is true, profiles and instances won't be loaded
// if the profile has no limits set on it, and nil will be returned.
func fetchProject(tx *db.ClusterTx, projectName string, skipIfNoLimits bool) (*api.Project, []db.Profile, []db.Instance, error) {
	project, err := tx.ProjectGet(projectName)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "Fetch project database object")
	}

	if skipIfNoLimits && !projectHasLimits(project) {
		return nil, nil, nil, nil
	}

	profilesFilter := db.ProfileFilter{}

	// If the project has the profiles feature enabled, we use its own
	// profiles to expand the instances configs, otherwise we use the
	// profiles from the default project.
	if projectName == Default || shared.IsTrue(project.Config["features.profiles"]) {
		profilesFilter.Project = projectName
	} else {
		profilesFilter.Project = Default
	}

	profiles, err := tx.ProfileList(profilesFilter)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "Fetch profiles from database")
	}

	instances, err := tx.InstanceList(db.InstanceFilter{Project: projectName})
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "Fetch project instances from database")
	}

	return project, profiles, instances, nil
}

// Expand the configuration of the given instances, taking the give project
// profiles into account.
func expandInstancesConfig(instances []db.Instance, profiles []db.Profile) []db.Instance {
	expandedInstances := make([]db.Instance, len(instances))

	// Index of all profiles by name.
	profilesByName := map[string]db.Profile{}
	for _, profile := range profiles {
		profilesByName[profile.Name] = profile
	}

	for i, instance := range instances {
		profiles := make([]api.Profile, len(instance.Profiles))

		for j, name := range instance.Profiles {
			profile := profilesByName[name]
			profiles[j] = *db.ProfileToAPI(&profile)
		}

		expandedInstances[i] = instance
		expandedInstances[i].Config = db.ProfilesExpandConfig(instance.Config, profiles)
	}

	return expandedInstances
}

// Sum of the effective instance-level value for the given limits across all
// project instances. If excludeInstance is not the empty string, exclude the
// instance with that name.
func getTotalsAcrossInstances(instances []db.Instance, keys []string) (map[string]int64, error) {
	totals := map[string]int64{}

	for _, key := range keys {
		totals[key] = 0
	}

	for _, instance := range instances {
		limits, err := getInstanceLimits(instance, keys)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			totals[key] += limits[key]
		}
	}

	return totals, nil
}

// Return the effective instance-level values for the limits with the given
// keys.
func getInstanceLimits(instance db.Instance, keys []string) (map[string]int64, error) {
	limits := map[string]int64{}

	for _, key := range keys {
		value, ok := instance.Config[key]
		if !ok || value == "" {
			return nil, fmt.Errorf(
				"Instance %s in project %s has no '%s' config, either directly or via a profile",
				instance.Name, instance.Project, key)
		}
		parser := aggregateLimitConfigValueParsers[key]
		limit, err := parser(value)
		if err != nil {
			return nil, errors.Wrapf(
				err, "Parse '%s' for instance %s in project %s",
				key, instance.Name, instance.Project)
		}
		limits[key] = limit
	}

	return limits, nil
}

var aggregateLimitConfigValueParsers = map[string]func(string) (int64, error){
	"limits.memory": func(value string) (int64, error) {
		if strings.HasSuffix(value, "%") {
			return -1, fmt.Errorf("Value can't be a percentage")
		}
		return units.ParseByteSizeString(value)
	},
	"limits.processes": func(value string) (int64, error) {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return -1, err
		}
		return int64(limit), nil
	},
	"limits.cpu": func(value string) (int64, error) {
		if strings.Contains(value, ",") || strings.Contains(value, "-") {
			return -1, fmt.Errorf("CPUs can't be pinned if project limits are used")
		}

		limit, err := strconv.Atoi(value)
		if err != nil {
			return -1, err
		}

		return int64(limit), nil
	},
}

var aggregateLimitConfigValuePrinters = map[string]func(int64) string{
	"limits.memory": func(limit int64) string {
		return units.GetByteSizeString(limit, 1)
	},
	"limits.processes": func(limit int64) string {
		return fmt.Sprintf("%d", limit)
	},
	"limits.cpu": func(limit int64) string {
		return fmt.Sprintf("%d", limit)
	},
}
