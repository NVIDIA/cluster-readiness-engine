// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bundle

// rowSchemaName is the schema file's name inside the bundle's schema
// directory.
const rowSchemaName = "component-row.schema.json"

// rowSchema describes one line of a components/*.ndjson file.
//
// Shipped inside the bundle rather than published separately, because the
// bundle is meant to be self-describing: a consumer handed a directory can
// validate its rows without knowing which NVCRE version wrote them, and an
// archived bundle stays readable after this repository has moved on.
//
// A string literal rather than an embedded file. go:embed would put the same
// bytes in the binary through a path that a reader of this package cannot see
// the contents of, and the schema is small enough that keeping it beside the
// Go type it describes is what stops the two drifting.
//
// The field list and the units mirror shard.Row. value is a number rendered to
// two decimal places by the writer, and is kept a JSON number here rather than
// a string so an arithmetic consumer needs no conversion step.
const rowSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://github.com/NVIDIA/cluster-readiness-engine/schema/component-row.schema.json",
  "title": "NVCRE per-component result row",
  "description": "One line of a components/*.ndjson file in an NVCRE result bundle (ADR-090). Each line is one measurement of one component on one node at one sample.",
  "type": "object",
  "additionalProperties": false,
  "required": ["node", "componentID", "index", "name", "value"],
  "properties": {
    "node": {
      "type": "string",
      "description": "Kubernetes node the component is attached to."
    },
    "componentID": {
      "type": "string",
      "description": "Stable identity of the component, such as a GPU UUID. Unique within the node."
    },
    "index": {
      "type": "integer",
      "minimum": 0,
      "description": "Device index of the component within its node."
    },
    "name": {
      "type": "string",
      "description": "Metric name, such as achievedTFLOPS."
    },
    "value": {
      "type": "number",
      "description": "Measured value, rendered to two decimal places."
    },
    "unit": {
      "type": "string",
      "description": "Unit of value, such as TFLOPS or GB/s."
    },
    "sample": {
      "type": "integer",
      "minimum": 0,
      "description": "Ordinal of this measurement within the attempt, for metrics sampled over time."
    },
    "timestamp": {
      "type": "string",
      "description": "When the measurement was taken, as the producer reported it."
    },
    "outcome": {
      "type": "string",
      "description": "The producer's verdict for this measurement, such as pass or fail."
    }
  }
}
`
