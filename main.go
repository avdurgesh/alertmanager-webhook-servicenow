package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"

	"gopkg.in/alecthomas/kingpin.v2"
	"gopkg.in/yaml.v2"

	"github.com/prometheus/common/log"
)

var (
	configFile    = kingpin.Flag("config.file", "ServiceNow configuration file.").Default("config/servicenow.yml").String()
	listenAddress = kingpin.Flag("web.listen-address", "The address to listen on for HTTP requests.").Default(":9877").String()
	config        Config
	serviceNow    ServiceNow
)

// Config - ServiceNow webhook configuration
type Config struct {
	ServiceNow      ServiceNowConfig      `yaml:"service_now"`
	DefaultIncident DefaultIncidentConfig `yaml:"default_incident"`
}

// ServiceNowConfig - ServiceNow instance configuration
type ServiceNowConfig struct {
	InstanceName          string `yaml:"instance_name"`
	UserName              string `yaml:"user_name"`
	Password              string `yaml:"password"`
	IncidentGroupKeyField string `yaml:"incident_group_key_field"`
}

// DefaultIncidentConfig - Default configuration for an incident
type DefaultIncidentConfig struct {
	AssignmentGroup string      `yaml:"assignment_group"`
	Impact          json.Number `yaml:"impact"`
	Urgency         json.Number `yaml:"urgency"`
}

// JSONResponse is the Webhook http response
type JSONResponse struct {
	Status  int
	Message string
}

func webhook(w http.ResponseWriter, r *http.Request) {

	data, err := readRequestBody(r)
	if err != nil {
		log.Errorf("Error reading request body : %v", err)
		sendJSONResponse(w, http.StatusBadRequest, err.Error())
		return
	}

	err = manageIncidents(data)

	if err != nil {
		log.Errorf("Error managing incident from alert : %v", err)
		sendJSONResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Returns a 200 if everything went smoothly
	sendJSONResponse(w, http.StatusOK, "Success")
}

// Starts 2 listeners
// - first one to give a status on the receiver itself
// - second one to actually process the data
func main() {
	kingpin.Version(version.Print("alertmanager-webhook-servicenow"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	config = loadConfig(*configFile)
	createSnClient(config)

	log.Info("Starting webhook", version.Info())
	log.Info("Build context", version.BuildContext())

	http.HandleFunc("/webhook", webhook)
	http.Handle("/metrics", promhttp.Handler())

	log.Infof("listening on: %v", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

func sendJSONResponse(w http.ResponseWriter, status int, message string) {
	data := JSONResponse{
		Status:  status,
		Message: message,
	}
	bytes, _ := json.Marshal(data)

	w.WriteHeader(status)
	_, err := w.Write(bytes)

	if err != nil {
		log.Errorf("Error writing JSON response: %s", err)
	}
}

func readRequestBody(r *http.Request) (template.Data, error) {

	// Do not forget to close the body at the end
	defer r.Body.Close()

	// Extract data from the body in the Data template provided by AlertManager
	data := template.Data{}
	err := json.NewDecoder(r.Body).Decode(&data)

	return data, err
}

func loadConfig(configFile string) Config {
	config := Config{}

	// Load the config from the file
	configData, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	errYAML := yaml.Unmarshal([]byte(configData), &config)
	if errYAML != nil {
		log.Fatalf("Error unmarshalling config file: %v", errYAML)
	}

	log.Info("ServiceNow config loaded")
	return config
}

func createSnClient(config Config) ServiceNow {
	var err error
	serviceNow, err = NewServiceNowClient(config.ServiceNow.InstanceName, config.ServiceNow.UserName, config.ServiceNow.Password, config.ServiceNow.IncidentGroupKeyField)
	if err != nil {
		log.Fatalf("Error creating the ServiceNow client: %v", err)
	}
	return serviceNow
}

func manageIncidents(data template.Data) error {

	log.Infof("Received alert group: Status=%s, GroupLabels=%v, CommonLabels=%v, CommonAnnotations=%v",
		data.Status, data.GroupLabels, data.CommonLabels, data.CommonAnnotations)

	groupKey := getGroupKey(data)
	getParams := map[string]string{
		config.ServiceNow.IncidentGroupKeyField: groupKey,
	}

	incidents, err := serviceNow.GetIncidents(getParams)

	if len(incidents) == 0 {
		log.Infof("Found no existing incident for alert group key: %s", groupKey)
		incident := dataToIncidentParam(data)
		if _, err = serviceNow.CreateIncident(incident); err != nil {
			return err
		}
	} else {
		if len(incidents) > 1 {
			log.Warnf("Found multiple existing incidents for alert group key: %s. Will use first one.", groupKey)
		}
		existingIncident := incidents[0]
		log.Infof("Found existing incident (%s) for alert group key: %s", existingIncident.GetNumber(), groupKey)
		incident := dataToIncidentParam(data)
		if _, err = serviceNow.UpdateIncident(incident, existingIncident.GetSysID()); err != nil {
			return err
		}
	}

	return nil
}

func dataToIncidentParam(data template.Data) IncidentParam {

	var shortDescriptionBuilder strings.Builder
	shortDescriptionBuilder.WriteString(fmt.Sprintf("[%s] ", data.Status))
	var groupKeyBuilder strings.Builder
	for _, label := range data.GroupLabels.SortedPairs() {
		if groupKeyBuilder.Len() > 0 {
			groupKeyBuilder.WriteString(", ")
		}
		groupKeyBuilder.WriteString(fmt.Sprintf("%s: %s", label.Name, label.Value))
	}
	shortDescriptionBuilder.WriteString(groupKeyBuilder.String())

	var descriptionBuilder strings.Builder
	descriptionBuilder.WriteString(fmt.Sprintf("Group key: %s", groupKeyBuilder.String()))
	descriptionBuilder.WriteString(fmt.Sprintf("\nAlertManager receiver: %s", data.Receiver))
	descriptionBuilder.WriteString(fmt.Sprintf("\nAlertManager source URL: %s", data.ExternalURL))

	var commentBuilder strings.Builder
	commentBuilder.WriteString("Alerts list:")
	for _, alert := range data.Alerts {
		var alertBuilder strings.Builder
		alertBuilder.WriteString(fmt.Sprintf("[%s] %v", alert.Status, alert.StartsAt))
		for _, label := range alert.Labels.SortedPairs() {
			alertBuilder.WriteString(fmt.Sprintf("\n- %s: %s", label.Name, label.Value))
		}
		for _, annotation := range alert.Annotations.SortedPairs() {
			alertBuilder.WriteString(fmt.Sprintf("\n- %s: %s", annotation.Name, annotation.Value))
		}
		commentBuilder.WriteString(fmt.Sprintf("\n\n%s", alertBuilder.String()))
	}

	incidentParam := IncidentParam{
		AssignmentGroup:  config.DefaultIncident.AssignmentGroup,
		CallerID:         config.ServiceNow.UserName,
		Comments:         commentBuilder.String(),
		Description:      descriptionBuilder.String(),
		Impact:           config.DefaultIncident.Impact,
		ShortDescription: shortDescriptionBuilder.String(),
		GroupKey:         getGroupKey(data),
		Urgency:          config.DefaultIncident.Urgency,
	}

	return incidentParam
}

func getGroupKey(data template.Data) string {
	return fmt.Sprintf("%v", data.GroupLabels.SortedPairs())
}
