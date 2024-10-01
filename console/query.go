// SPDX-FileCopyrightText: 2022 Free Mobile
// SPDX-License-Identifier: AGPL-3.0-only

package console

import (
	"fmt"
	"strings"

	"akvorado/common/schema"
	"akvorado/console/query"
)

func requireMainTable(sch *schema.Component, qcs []query.Column, qf query.Filter) bool {
	if qf.MainTableRequired() {
		return true
	}
	for _, qc := range qcs {
		if column, ok := sch.LookupColumnByKey(qc.Key()); ok && column.ClickHouseMainOnly {
			return true
		}
	}
	return false
}

// fixQueryColumnName fix capitalization of the provided column name
func (c *Component) fixQueryColumnName(name string) string {
	name = strings.ToLower(name)
	for _, column := range c.d.Schema.Columns() {
		if strings.ToLower(column.Name) == name {
			return column.Name
		}
	}
	return ""
}

func metricForTopSort(inputUnit string) string {
	switch inputUnit {
	case "pps":
		return "Packets"
	default:
		return "Bytes"
	}
}

func selectSankeyRowsByLimitType(input graphSankeyHandlerInput, dimensions []string, where string) string {
	return selectRowsByLimitType(input.graphCommonHandlerInput, dimensions, where)
}

func selectLineRowsByLimitType(input graphLineHandlerInput, dimensions []string, where string) string {
	return selectRowsByLimitType(input.graphCommonHandlerInput, dimensions, where)
}

func selectRowsByLimitType(input graphCommonHandlerInput, dimensions []string, where string) string {
	var rowsType string
	if input.LimitType == "Max" {
		rowsType = fmt.Sprintf(
			"rows AS (SELECT %s FROM ( SELECT %s AS max_bytes_at_time FROM source WHERE %s GROUP BY %s ) GROUP BY %s ORDER BY MAX(%s) DESC LIMIT %d)",
			strings.Join(dimensions, ", "),
			strings.Join(append(dimensions, fmt.Sprintf("MAX(%s)", metricForTopSort(input.Units))), ", "),
			where,
			strings.Join(append(dimensions, "{{ .Timefilter }}"), ", "),
			strings.Join(dimensions, ", "),
			"max_bytes_at_time",
			input.Limit)
	} else {
		rowsType = fmt.Sprintf(
			"rows AS (SELECT %s FROM source WHERE %s GROUP BY %s ORDER BY SUM(%s) DESC LIMIT %d)",
			strings.Join(dimensions, ", "),
			where,
			strings.Join(dimensions, ", "),
			metricForTopSort(input.Units),
			input.Limit)
	}
	return rowsType
}
