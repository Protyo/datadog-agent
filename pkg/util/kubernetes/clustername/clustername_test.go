// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2019 Datadog, Inc.

package clustername

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/DataDog/datadog-agent/pkg/config"
)

func TestGetClusterName(t *testing.T) {
	mockConfig := config.Mock()
	data := newClusterNameData()

	var testClusterName = "laika"
	mockConfig.Set("cluster_name", testClusterName)
	defer mockConfig.Set("cluster_name", nil)

	assert.Equal(t, testClusterName, getClusterName(data))

	// Test caching and reset
	var newClusterName = "youri"
	mockConfig.Set("cluster_name", newClusterName)
	assert.Equal(t, testClusterName, getClusterName(data))
	freshData := newClusterNameData()
	assert.Equal(t, newClusterName, getClusterName(freshData))

	// Test invalid cluster names
	for _, invalidClusterName := range []string{
		"Capital",
		"with_underscore",
		"toolongtoolongtoolongtoolongtoolongtoolong"} {
		mockConfig.Set("cluster_name", invalidClusterName)
		freshData = newClusterNameData()
		assert.Equal(t, "", getClusterName(freshData))
	}
}
