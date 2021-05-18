/*
 * Copyright NetApp Inc, 2021 All rights reserved
 */
package main

import (
	"goharvest2/cmd/poller/collector"
	"goharvest2/cmd/poller/plugin"
	"goharvest2/pkg/api/ontapi/zapi"
	"goharvest2/pkg/dict"
	"goharvest2/pkg/errors"
	"goharvest2/pkg/logger"
	"goharvest2/pkg/matrix"
	"goharvest2/pkg/tree/node"
	"strings"
)

type Shelf struct {
	*plugin.AbstractPlugin
	data           map[string]*matrix.Matrix
	instanceKeys   map[string]string
	instanceLabels map[string]*dict.Dict
	client         *zapi.Client
	query          string
}

func New(p *plugin.AbstractPlugin) plugin.Plugin {
	return &Shelf{AbstractPlugin: p}
}

func (my *Shelf) Init() error {

	var err error

	if err = my.InitAbc(); err != nil {
		return err
	}

	if my.client, err = zapi.New(my.ParentParams); err != nil {
		logger.Error(my.Prefix, "connecting: %v", err)
		return err
	}

	if err = my.client.Init(5); err != nil {
		return err
	}

	if my.client.IsClustered() {
		my.query = "storage-shelf-info-get-iter"
	} else {
		my.query = "storage-shelf-environment-list-info"
	}

	logger.Debug(my.Prefix, "plugin connected!")

	my.data = make(map[string]*matrix.Matrix)
	my.instanceKeys = make(map[string]string)
	my.instanceLabels = make(map[string]*dict.Dict)

	objects := my.Params.GetChildS("objects")
	if objects == nil {
		return errors.New(errors.MissingParam, "objects")
	}

	for _, obj := range objects.GetChildren() {

		attribute := obj.GetNameS()
		objectName := strings.ReplaceAll(attribute, "-", "_")

		if x := strings.Split(attribute, "=>"); len(x) == 2 {
			attribute = strings.TrimSpace(x[0])
			objectName = strings.TrimSpace(x[1])
		}

		my.instanceLabels[attribute] = dict.New()

		my.data[attribute] = matrix.New(my.Parent+".Shelf", "shelf_"+objectName)
		my.data[attribute].SetGlobalLabel("datacenter", my.ParentParams.GetChildContentS("datacenter"))
		my.data[attribute].SetGlobalLabel("cluster", my.client.Name())

		exportOptions := node.NewS("export_options")
		instanceLabels := exportOptions.NewChildS("instance_labels", "")
		instanceKeys := exportOptions.NewChildS("instance_keys", "")
		instanceKeys.NewChildS("", "shelf")

		for _, x := range obj.GetChildren() {

			for _, c := range x.GetAllChildContentS() {

				metricName, display := collector.ParseMetricName(c)

				if strings.HasPrefix(c, "^") {
					if strings.HasPrefix(c, "^^") {
						my.instanceKeys[attribute] = metricName
						my.instanceLabels[attribute].Set(metricName, display)
						instanceKeys.NewChildS("", display)
						logger.Debug(my.Prefix, "added instance key: (%s) (%s) [%s]", attribute, x.GetNameS(), display)
					} else {
						my.instanceLabels[attribute].Set(metricName, display)
						instanceLabels.NewChildS("", display)
						logger.Debug(my.Prefix, "added instance label: (%s) (%s) [%s]", attribute, x.GetNameS(), display)
					}
				} else {
					metric, err := my.data[attribute].NewMetricFloat64(metricName)
					if err != nil {
						logger.Error(my.Prefix, "add metric: %v", err)
						return err
					}
					metric.SetName(display)
					logger.Debug(my.Prefix, "added metric: (%s) (%s) [%s]", attribute, x.GetNameS(), display)
				}
			}
		}
		logger.Debug(my.Prefix, "added data for [%s] with %d metrics", attribute, len(my.data[attribute].GetMetrics()))

		my.data[attribute].SetExportOptions(exportOptions)
	}

	logger.Debug(my.Prefix, "initialized with data [%d] objects", len(my.data))
	return nil
}

func (my *Shelf) Run(data *matrix.Matrix) ([]*matrix.Matrix, error) {

	var (
		result  *node.Node
		shelves []*node.Node
		err     error
	)

	if !my.client.IsClustered() {
		for _, instance := range data.GetInstances() {
			instance.SetLabel("shelf", instance.GetLabel("shelf_id"))
		}
	}

	if result, err = my.client.InvokeRequestString(my.query); err != nil {
		return nil, err
	}

	if x := result.GetChildS("attributes-list"); x != nil {
		shelves = x.GetChildren()
	} else if !my.client.IsClustered() {
		//fallback to 7mode
		shelves = result.SearchChildren([]string{"shelf-environ-channel-info", "shelf-environ-shelf-list", "shelf-environ-shelf-info"})
	}

	if len(shelves) == 0 {
		return nil, errors.New(errors.NoInstancesError, "no shelf instances found")
	}

	logger.Debug(my.Prefix, "fetching %d shelf counters", len(shelves))

	output := make([]*matrix.Matrix, 0)

	for _, shelf := range shelves {

		shelfName := shelf.GetChildContentS("shelf")
		shelfId := shelf.GetChildContentS("shelf-uid")

		if !my.client.IsClustered() {
			uid := shelf.GetChildContentS("shelf-id")
			shelfName = uid // no shelf name in 7mode
			shelfId = uid
		}

		for attribute, data := range my.data {

			data.PurgeInstances()

			if my.instanceKeys[attribute] == "" {
				logger.Warn(my.Prefix, "no instance keys defined for object [%s], skipping", attribute)
				continue
			}

			objectElem := shelf.GetChildS(attribute)
			if objectElem == nil {
				logger.Warn(my.Prefix, "no [%s] instances on this system", attribute)
				continue
			}

			logger.Debug(my.Prefix, "fetching %d [%s] instances", len(objectElem.GetChildren()), attribute)

			for _, obj := range objectElem.GetChildren() {

				if key := obj.GetChildContentS(my.instanceKeys[attribute]); key != "" {

					instance, err := data.NewInstance(shelfId + "." + key)

					if err != nil {
						logger.Debug(my.Prefix, "add (%s) instance: %v", attribute, err)
						return nil, err
					}

					logger.Debug(my.Prefix, "add (%s) instance: %s.%s", attribute, shelfId, key)

					for label, labelDisplay := range my.instanceLabels[attribute].Map() {
						if value := obj.GetChildContentS(label); value != "" {
							instance.SetLabel(labelDisplay, value)
						}
					}

					instance.SetLabel("shelf", shelfName)
					instance.SetLabel("shelf_id", shelfId)

				} else {
					logger.Debug(my.Prefix, "instance without [%s], skipping", my.instanceKeys[attribute])
				}
			}

			output = append(output, data)
		}
	}

	// second loop to populate numeric data

	for _, shelf := range shelves {

		shelfId := shelf.GetChildContentS("shelf-uid")
		if !my.client.IsClustered() {
			shelfId = shelf.GetChildContentS("shelf-id")
		}

		for attribute, data := range my.data {

			data.Reset()

			objectElem := shelf.GetChildS(attribute)
			if objectElem == nil {
				continue
			}

			for _, obj := range objectElem.GetChildren() {

				key := obj.GetChildContentS(my.instanceKeys[attribute])

				if key == "" {
					continue
				}

				instance := data.GetInstance(shelfId + "." + key)

				if instance == nil {
					logger.Debug(my.Prefix, "(%s) instance [%s.%s] not found in cache skipping", attribute, shelfId, key)
					continue
				}

				for metricKey, m := range data.GetMetrics() {

					if value := strings.Split(obj.GetChildContentS(metricKey), " ")[0]; value != "" {
						if err := m.SetValueString(instance, value); err != nil {
							logger.Debug(my.Prefix, "(%s) failed to parse value (%s): %v", metricKey, value, err)
						} else {
							logger.Debug(my.Prefix, "(%s) added value (%s)", metricKey, value)
						}
					}
				}
			}
		}
	}

	return output, nil
}

// Need to appease go build - see https://github.com/golang/go/issues/20312
func main() {}
