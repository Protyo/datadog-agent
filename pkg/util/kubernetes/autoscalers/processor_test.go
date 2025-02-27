// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2017 Datadog, Inc.

// +build kubeapiserver

package autoscalers

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/zorkian/go-datadog-api.v2"
	autoscalingv2 "k8s.io/api/autoscaling/v2beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/DataDog/datadog-agent/pkg/clusteragent/custommetrics"
)

type fakeDatadogClient struct {
	queryMetricsFunc func(from, to int64, query string) ([]datadog.Series, error)
}

func (d *fakeDatadogClient) QueryMetrics(from, to int64, query string) ([]datadog.Series, error) {
	if d.queryMetricsFunc != nil {
		return d.queryMetricsFunc(from, to, query)
	}
	return nil, nil
}

var maxAge = time.Duration(30 * time.Second)

func makePoints(ts, val int) datadog.DataPoint {
	if ts == 0 {
		ts = (int(metav1.Now().Unix()) - int(maxAge.Seconds())) * 1000 // use ms
	}
	tsPtr := float64(ts)
	valPtr := float64(val)
	return datadog.DataPoint{&tsPtr, &valPtr}
}

func makePartialPoints(ts int) datadog.DataPoint {
	tsPtr := float64(ts)
	return datadog.DataPoint{&tsPtr, nil}
}

func makePtr(val string) *string {
	return &val
}

func TestProcessor_UpdateExternalMetrics(t *testing.T) {
	penTime := (int(time.Now().Unix()) - int(maxAge.Seconds()/2)) * 1000
	metricName := "requests_per_s"
	tests := []struct {
		desc     string
		metrics  map[string]custommetrics.ExternalMetricValue
		series   []datadog.Series
		expected map[string]custommetrics.ExternalMetricValue
	}{
		{
			"update invalid metric",
			map[string]custommetrics.ExternalMetricValue{
				"id1": {
					MetricName: metricName,
					Labels:     map[string]string{"foo": "bar"},
					Valid:      false,
				},
			},
			[]datadog.Series{
				{
					Metric: &metricName,
					Points: []datadog.DataPoint{
						makePoints(1531492452000, 12),
						makePoints(penTime, 14), // Force the penultimate point to be considered fresh at all time(< externalMaxAge)
						makePoints(0, 27),
					},
					Scope: makePtr("foo:bar"),
				},
			},
			map[string]custommetrics.ExternalMetricValue{
				"id1": {
					MetricName: "requests_per_s",
					Labels:     map[string]string{"foo": "bar"},
					Value:      14,
					Valid:      true,
				},
			},
		},
		{
			"do not update valid sparse metric",
			map[string]custommetrics.ExternalMetricValue{
				"id2": {
					MetricName: "requests_per_s",
					Labels:     map[string]string{"2foo": "bar"},
					Valid:      true,
				},
			},
			[]datadog.Series{
				{
					Metric: &metricName,
					Points: []datadog.DataPoint{
						makePoints(1431492452000, 12),
						makePoints(1431492453000, 14), // Force the point to be considered outdated at all time(> externalMaxAge)
						makePoints(0, 1000),           // Force the point to be considered fresh at all time(< externalMaxAge)
					},
					Scope: makePtr("2foo:bar"),
				},
			},
			map[string]custommetrics.ExternalMetricValue{
				"id2": {
					MetricName: "requests_per_s",
					Labels:     map[string]string{"2foo": "bar"},
					Value:      14,
					Valid:      false,
				},
			},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("#%d %s", i, tt.desc), func(t *testing.T) {
			datadogClient := &fakeDatadogClient{
				queryMetricsFunc: func(int64, int64, string) ([]datadog.Series, error) {
					return tt.series, nil
				},
			}
			hpaCl := &Processor{datadogClient: datadogClient, externalMaxAge: maxAge}

			externalMetrics := hpaCl.UpdateExternalMetrics(tt.metrics)
			fmt.Println(externalMetrics)
			// Timestamps are always set to time.Now() so we cannot assert the value
			// in a unit test.
			strippedTs := make(map[string]custommetrics.ExternalMetricValue)
			for id, m := range externalMetrics {
				m.Timestamp = 0
				strippedTs[id] = m
			}
			fmt.Println(strippedTs)
			for id, m := range tt.expected {
				require.True(t, reflect.DeepEqual(m, strippedTs[id]))
			}
		})
	}

	// Test that Datadog not responding yields invaldation.
	emList := map[string]custommetrics.ExternalMetricValue{
		"id1": {
			MetricName: metricName,
			Labels:     map[string]string{"foo": "bar"},
			Valid:      true,
		},
		"id2": {
			MetricName: metricName,
			Labels:     map[string]string{"bar": "baz"},
			Valid:      true,
		},
	}
	datadogClient := &fakeDatadogClient{
		queryMetricsFunc: func(int64, int64, string) ([]datadog.Series, error) {
			return nil, fmt.Errorf("API error 400 Bad Request: {\"error\": [\"Rate limit of 300 requests in 3600 seconds reqchec.\"]}")
		},
	}
	hpaCl := &Processor{datadogClient: datadogClient, externalMaxAge: maxAge}
	invList := hpaCl.UpdateExternalMetrics(emList)
	require.Len(t, invList, len(emList))
	for _, i := range invList {
		require.False(t, i.Valid)
	}

}

func TestProcessor_ProcessHPAs(t *testing.T) {
	metricName := "requests_per_s"
	tests := []struct {
		desc     string
		metrics  autoscalingv2.HorizontalPodAutoscaler
		expected map[string]custommetrics.ExternalMetricValue
	}{
		{
			"process valid hpa external metric",
			autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "foo",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					Metrics: []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.ExternalMetricSourceType,
							External: &autoscalingv2.ExternalMetricSource{
								MetricName: metricName,
								MetricSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"dcos_version": "1.9.4",
									},
								},
							},
						},
					},
				},
			},
			map[string]custommetrics.ExternalMetricValue{
				"external_metric-default-foo-requests_per_s": {
					MetricName: "requests_per_s",
					Labels:     map[string]string{"dcos_version": "1.9.4"},
					Value:      0,
					Valid:      false,
				},
			},
		},
		{
			"process hpa external metrics",
			autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "foo",
				},
				Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
					Metrics: []autoscalingv2.MetricSpec{
						{
							Type: autoscalingv2.ExternalMetricSourceType,
							External: &autoscalingv2.ExternalMetricSource{
								MetricName: "m1",
								MetricSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"dcos_version": "1.9.4",
									},
								},
							},
						},
						{
							Type: autoscalingv2.ExternalMetricSourceType,
							External: &autoscalingv2.ExternalMetricSource{
								MetricName: "m2",
								MetricSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"dcos_version": "2.1.9",
									},
								},
							},
						},
						{
							Type: autoscalingv2.ExternalMetricSourceType,
							External: &autoscalingv2.ExternalMetricSource{
								MetricName: metricName,
								MetricSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"dcos_version": "4.1.1",
									},
								},
							},
						},
					},
				},
			},
			map[string]custommetrics.ExternalMetricValue{
				"external_metric-default-foo-m1": {
					MetricName: "m1",
					Labels:     map[string]string{"dcos_version": "1.9.4"},
					Value:      0,
					Valid:      false,
				},
				"external_metric-default-foo-m2": {
					MetricName: "m2",
					Labels:     map[string]string{"dcos_version": "2.1.9"},
					Value:      0,
					Valid:      false,
				},
				"external_metric-default-foo-m3": {
					MetricName: "requests_per_s",
					Labels:     map[string]string{"dcos_version": "4.1.1"},
					Value:      0, // If Datadog does not even return the metric, store it as invalid with Value = 0
					Valid:      false,
				},
			},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("#%d %s", i, tt.desc), func(t *testing.T) {
			datadogClient := &fakeDatadogClient{}
			hpaCl := &Processor{datadogClient: datadogClient, externalMaxAge: maxAge}

			externalMetrics := hpaCl.ProcessHPAs(&tt.metrics)
			for id, m := range externalMetrics {
				require.True(t, reflect.DeepEqual(m, externalMetrics[id]))
			}
		})
	}
}

// Test that we consistently get the same key.
func TestGetKey(t *testing.T) {
	tests := []struct {
		desc     string
		name     string
		labels   map[string]string
		expected string
	}{
		{
			"correct name and label",
			"kubernetes.io",
			map[string]string{
				"foo": "bar",
			},
			"kubernetes.io{foo:bar}",
		},
		{
			"correct name and labels",
			"kubernetes.io",
			map[string]string{
				"zfoo": "bar",
				"afoo": "bar",
				"ffoo": "bar",
			},
			"kubernetes.io{afoo:bar,ffoo:bar,zfoo:bar}",
		},
		{
			"correct name, no labels",
			"kubernetes.io",
			nil,
			"kubernetes.io{*}",
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			formatedKey := getKey(test.name, test.labels)
			require.Equal(t, test.expected, formatedKey)
		})
	}
}

func TestInvalidate(t *testing.T) {
	eml := map[string]custommetrics.ExternalMetricValue{
		"foo": {
			MetricName: "foo",
			Valid:      false,
			Timestamp:  12,
		},
		"bar": {
			MetricName: "bar",
			Valid:      true,
			Timestamp:  1300,
		},
	}

	invalid := invalidate(eml)
	for _, e := range invalid {
		require.False(t, e.Valid)
		require.WithinDuration(t, time.Now(), time.Unix(e.Timestamp, 0), 5*time.Second)
	}
}
