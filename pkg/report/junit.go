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
}

type junitIssue struct {
	Message string `xml:"message,attr"`
	Details string `xml:",chardata"`
}

// WriteJUnit writes one test suite per certification and one test case per
// category, or a certification error when no categories are available.
func WriteJUnit(path string, reports []*CertReport) error {
	var output junitSuites
	for _, r := range reports {
		suite := junitSuite{Name: r.Name}
		occurrences := make(map[string]int)
		var seconds float64
		for _, cat := range r.Categories {
			tc := junitCase{Name: cat.Variant, Classname: r.Name + "." + cat.Domain}
			identity := cat.Domain + "/" + cat.Variant
			occurrences[identity]++
			if occurrences[identity] > 1 {
				tc.Name = fmt.Sprintf("%s [%d]", cat.Variant, occurrences[identity])
			}
			if duration, err := time.ParseDuration(strings.ReplaceAll(cat.Runtime, " ", "")); err == nil {
				tc.Time = strconv.FormatFloat(duration.Seconds(), 'f', -1, 64)
				seconds += duration.Seconds()
			}
			var issue *junitIssue
			var details any = cat
			switch cat.Status {
			case statusSucceeded:
				if r.Result == "INCOMPLETE" || r.Result == "RUNNING" {
					issue = &junitIssue{Message: "Certification did not complete: " + r.Result}
					tc.Error = issue
					suite.Errors++
					details = r
				}
			case statusFailed:
				message := cat.FailureReason
				if message == "" {
					message = "Category failed"
				}
				issue = &junitIssue{Message: message}
				tc.Failure = issue
				suite.Failures++
			default:
				issue = &junitIssue{Message: "Category did not complete: " + cat.Status}
				tc.Error = issue
				suite.Errors++
			}
			if issue != nil {
				data, err := json.MarshalIndent(details, "", "  ")
				if err != nil {
					return fmt.Errorf("marshal category report: %w", err)
				}
				issue.Details = string(data)
			}
			suite.Cases = append(suite.Cases, tc)
		}
		if len(r.Categories) == 0 {
			data, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal certification report: %w", err)
			}
			suite.Cases = append(suite.Cases, junitCase{
				Name: "certification", Classname: r.Name,
				Error: &junitIssue{Message: "Certification did not complete: " + r.Result, Details: string(data)},
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
