package plugin

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/samanamonitor/samm-citrixodata-datasource/pkg/plugin/odata"
	"github.com/samanamonitor/samm-citrixodata-datasource/pkg/plugin/citrixcloud"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"golang.org/x/oauth2/clientcredentials"
	"golang.org/x/oauth2"
)

var (
	_ backend.QueryDataHandler    = (*ODataSource)(nil)
	_ backend.CheckHealthHandler  = (*ODataSource)(nil)
	_ backend.CallResourceHandler = (*ODataSource)(nil)
)

type ODataSource struct {
	im instancemgmt.InstanceManager
}

type DatasourceSettings struct {
	URLSpaceEncoding string `json:"urlSpaceEncoding"`
	CitrixCloudUrl   string `json:"citrixCloudUrl"`
	AuthUrl          string `json:"authUrl"`
	CustomerId       string `json:"customerId"`
	ClientId         string `json:"clientId"`
}

func newDatasourceInstance(ctx context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	var dsSettings DatasourceSettings
	if settings.JSONData != nil && len(settings.JSONData) > 1 {
		if err := json.Unmarshal(settings.JSONData, &dsSettings); err != nil {
			return nil, err
		}
	}
	config := clientcredentials.Config{
		ClientID:       dsSettings.ClientId,
		ClientSecret:   settings.DecryptedSecureJSONData["clientSecret"],
		TokenURL:       dsSettings.AuthUrl,
		Scopes:         []string{},
		EndpointParams: url.Values{},
		AuthStyle:      oauth2.AuthStyleInParams,
	}
	ctx = context.WithValue(context.Background(), oauth2.HTTPClient, nil)
	client := config.Client(ctx)
	client.Transport = &citrixcloud.Transport{
		Source: config.TokenSource(ctx),
		Base:   nil,
	}

	return &ODataSourceInstance{
		&ODataClientImpl{client, dsSettings.CitrixCloudUrl, dsSettings.URLSpaceEncoding, dsSettings.CustomerId},
	}, nil
}

type ODataSourceInstance struct {
	client ODataClient
}

func NewODataSource(ctx context.Context, _ backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	im := datasource.NewInstanceManager(newDatasourceInstance)
	ds := &ODataSource{
		im: im,
	}
	return ds, nil
}

func (ds *ODataSource) getClientInstance(ctx context.Context, pluginContext backend.PluginContext) ODataClient {
	instance, _ := ds.im.Get(ctx, pluginContext)
	clientInstance := instance.(*ODataSourceInstance).client
	return clientInstance
}

func (ds *ODataSource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse,
	error) {
	clientInstance := ds.getClientInstance(ctx, req.PluginContext)
	response := backend.NewQueryDataResponse()
	for _, q := range req.Queries {
		res := ds.query(clientInstance, q)
		response.Responses[q.RefID] = res
	}
	return response, nil
}

func (ds *ODataSource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult,
	error) {
	var status backend.HealthStatus
	var message string
	clientInstance := ds.getClientInstance(ctx, req.PluginContext)
	var res, err = clientInstance.GetServiceRoot()
	if err != nil {
		status = backend.HealthStatusError
		message = fmt.Sprintf("Health check failed: %s", err.Error())
	} else {
		if res.StatusCode == 200 {
			status = backend.HealthStatusOk
			message = "Data Source is working as expected."
		} else {
			status = backend.HealthStatusError
			message = fmt.Sprintf("Health check failed, datasource exists but given path does not. "+
				"Statuscode: %d", res.StatusCode)
		}
	}
	return &backend.CheckHealthResult{
		Status:  status,
		Message: message,
	}, nil
}

func (ds *ODataSource) CallResource(ctx context.Context, req *backend.CallResourceRequest,
	sender backend.CallResourceResponseSender) error {
	switch req.Path {
	case "metadata":
		return ds.getMetadata(ctx, req, sender)
	default:
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusNotFound,
		})
	}
}

func (ds *ODataSource) query(clientInstance ODataClient, query backend.DataQuery) backend.DataResponse {
	log.DefaultLogger.Debug("query", "query.JSON", string(query.JSON))
	response := backend.DataResponse{}
	var qm queryModel
	err := json.Unmarshal(query.JSON, &qm)
	if err != nil {
		response.Error = fmt.Errorf("error unmarshalling query json: %w", err)
		return response
	}

	// Prevent empty queries from being executed
	if qm.TimeProperty == nil && len(qm.Properties) == 0 {
		return response
	}

	frame := data.NewFrame("response")
	frame.Name = query.RefID
	if frame.Meta == nil {
		frame.Meta = &data.FrameMeta{}
	}
	frame.Meta.PreferredVisualization = data.VisTypeTable

	if qm.Count {
		field := data.NewField("Count", nil, []*int64{})
		frame.Fields = append(frame.Fields, field)
	} else {
		if qm.TimeProperty != nil {
			log.DefaultLogger.Debug("Time property configured", "name", qm.TimeProperty.Name)
			labels, err := data.LabelsFromString("time=" + qm.TimeProperty.Name)
			if err != nil {
				response.Error = err
				return response
			}
			field := data.NewField(qm.TimeProperty.Name, labels, odata.ToArray(qm.TimeProperty.Type))
			frame.Fields = append(frame.Fields, field)
		}
		for _, prop := range qm.Properties {
			field := data.NewField(prop.Name, nil, odata.ToArray(prop.Type))
			frame.Fields = append(frame.Fields, field)
		}
	}

	props := qm.Properties
	if qm.TimeProperty != nil {
		props = append(props, *qm.TimeProperty)
	}
	resp, err := clientInstance.Get(qm.EntitySet.Name, props,
		append(qm.FilterConditions, TimeRangeToFilter(query.TimeRange, qm.TimeProperty)...), qm.Count)
	if err != nil {
		response.Error = err
		return response
	}

	defer resp.Body.Close()

	log.DefaultLogger.Debug("request response status", "status", resp.Status)
	if resp.StatusCode != http.StatusOK {
		response.Error = fmt.Errorf("get failed with status code %d", resp.StatusCode)
		return response
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		response.Error = err
		return response
	}
	var result odata.Response
	err = json.Unmarshal(bodyBytes, &result)
	if err != nil {
		response.Error = err
		return response
	}

	log.DefaultLogger.Debug("query complete", "noOfEntities", len(result.Value))

	if qm.Count {
		var values []interface{}
		values = make([]interface{}, 1)
		values[0] = &result.Count
		frame.AppendRow(values...)
	} else {
		for _, entry := range result.Value {
			var values []interface{}

			if qm.TimeProperty != nil {
				values = make([]interface{}, len(qm.Properties)+1)
				values[0] = odata.MapValue(entry[qm.TimeProperty.Name], qm.TimeProperty.Type)
			} else {
				values = make([]interface{}, len(qm.Properties))
			}

			for i, prop := range qm.Properties {
				index := i
				if qm.TimeProperty != nil {
					index++
				}
				if value, ok := entry[prop.Name]; ok {
					values[index] = odata.MapValue(value, prop.Type)
				} else {
					values[index] = nil
				}
			}
			frame.AppendRow(values...)
		}
	}

	response.Frames = append(response.Frames, frame)
	return response
}

func (ds *ODataSource) getMetadata(ctx context.Context, req *backend.CallResourceRequest,
	sender backend.CallResourceResponseSender) error {
	clientInstance := ds.getClientInstance(ctx, req.PluginContext)
	resp, err := clientInstance.GetMetadata()

	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get metadata failed with status code %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.DefaultLogger.Error("error reading response body")
		return err
	}
	var edmx odata.Edmx
	err = xml.Unmarshal(bodyBytes, &edmx)
	if err != nil {
		log.DefaultLogger.Error("error unmarshalling response body")
		return err
	}

	metadata := schema{
		EntityTypes: make(map[string]entityType),
		EntitySets:  make(map[string]entitySet),
	}
	for _, ds := range edmx.DataServices {
		for _, s := range ds.Schemas {
			for _, et := range s.EntityTypes {
				qualifiedName := s.Namespace + "." + et.Name
				var properties []property
				for _, p := range et.Properties {
					prop := property{
						Name: p.Name,
						Type: p.Type,
					}
					properties = append(properties, prop)
				}
				metadata.EntityTypes[qualifiedName] = entityType{
					Name:          et.Name,
					QualifiedName: qualifiedName,
					Properties:    properties,
				}
			}
			for _, ec := range s.EntityContainers {
				for _, es := range ec.EntitySet {
					metadata.EntitySets[es.Name] = entitySet{
						Name:       es.Name,
						EntityType: es.EntityType,
					}
				}
			}
		}
	}

	responseBody, err := json.Marshal(metadata)
	if err != nil {
		log.DefaultLogger.Error("error marshalling response body")
		return err
	}
	return sender.Send(&backend.CallResourceResponse{
		Status: http.StatusOK,
		Body:   responseBody,
	})
}
