// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	junitPassed     = "PASSED"
	junitFailed     = "FAILED"
	junitIncomplete = "INCOMPLETE"
)

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string      `xml:"name,attr"`
	Classname string      `xml:"classname,attr"`
	Time      string      `xml:"time,attr,omitempty"`
	Failure   *junitIssue `xml:"failure,omitempty"`
	Error     *junitIssue `xml:"error,omitempty"`
	SystemOut string      `xml:"system-out"`
}

type junitIssue struct {
	Message string `xml:"message,attr"`
	Details string `xml:",chardata"`
}

// WriteJUnit writes one test suite per certification and one test case per
// category. Incomplete reports retain their available results and include an
// error, so an untested or partially tested certification cannot appear passed.
func WriteJUnit(path string, reports []*CertReport) error {
	if len(reports) == 0 {
		return fmt.Errorf("no certification reports to write")
	}
	var output junitSuites
	for _, r := range reports {
		if r == nil {
			return fmt.Errorf("nil certification report")
		}
		suite := junitSuite{Name: r.Name}
		var seconds float64
		for _, cat := range r.Categories {
			data, err := json.MarshalIndent(cat, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal category report: %w", err)
			}
			tc := junitCase{Name: cat.Variant, Classname: r.Name + "." + cat.Domain, SystemOut: string(data)}
			if duration, err := time.ParseDuration(strings.ReplaceAll(cat.Runtime, " ", "")); err == nil && duration >= 0 {
				tc.Time = strconv.FormatFloat(duration.Seconds(), 'f', -1, 64)
				seconds += duration.Seconds()
			}
			switch cat.Status {
			case statusSucceeded:
			case statusFailed:
				message := cat.FailureReason
				if message == "" {
					message = "Category failed"
				}
				tc.Failure = &junitIssue{Message: message, Details: string(data)}
				suite.Failures++
			default:
				tc.Error = &junitIssue{Message: "Category did not complete: " + cat.Status, Details: string(data)}
				suite.Errors++
			}
			suite.Cases = append(suite.Cases, tc)
		}
		if len(r.Categories) == 0 || r.Result == junitIncomplete ||
			(r.Result != junitPassed && r.Result != junitFailed && suite.Errors == 0) {
			data, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal certification report: %w", err)
			}
			suite.Cases = append(suite.Cases, junitCase{
				Name: "certification", Classname: r.Name,
				Error:     &junitIssue{Message: "Certification did not complete: " + r.Result, Details: string(data)},
				SystemOut: string(data),
			})
			suite.Errors++
		}
		suite.Tests = len(suite.Cases)
		suite.Time = strconv.FormatFloat(seconds, 'f', -1, 64)
		output.Suites = append(output.Suites, suite)
	}
	data, err := xml.MarshalIndent(output, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JUnit report: %w", err)
	}
	data = append([]byte(xml.Header), data...)
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil { // #nosec G306 -- reports are output files meant to be readable
		return fmt.Errorf("write JUnit report file: %w", err)
	}
	return nil
}
