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
)

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Time       string          `xml:"time,attr"`
	Properties []junitProperty `xml:"properties>property"`
	Cases      []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
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
// category, plus a certification-level case when the categories alone do not
// carry the certification's result (ADR-108).
func WriteJUnit(path string, reports []*CertReport) error {
	var output junitSuites
	for _, r := range reports {
		suite, err := junitSuiteFor(r)
		if err != nil {
			return err
		}
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

// junitSuiteFor maps one certification to a suite. A category's case reports
// that category's own outcome, so a category that finished stays passed
// whatever the certification did afterwards. A category that had not finished
// is an error while the certification runs, and a failure once the
// certification has failed, because it never will finish.
//
// INCOMPLETE adds no error. Build reports it with exit 0 since the excluded
// nodes were never tested, so the suite records them as properties instead.
func junitSuiteFor(r *CertReport) (junitSuite, error) {
	suite := junitSuite{Name: r.Name, Properties: junitProperties(r)}
	occurrences := make(map[string]int)
	var seconds int64
	for _, cat := range r.Categories {
		tc := junitCase{Name: cat.Variant, Classname: r.Name + "." + cat.Domain}
		identity := cat.Domain + "/" + cat.Variant
		occurrences[identity]++
		if occurrences[identity] > 1 {
			tc.Name = fmt.Sprintf("%s [%d]", cat.Variant, occurrences[identity])
		}
		if cat.RuntimeSeconds > 0 {
			tc.Time = strconv.FormatInt(cat.RuntimeSeconds, 10)
			seconds += cat.RuntimeSeconds
		}
		var issue *junitIssue
		switch {
		case cat.Status == statusSucceeded:
		case cat.Status == statusFailed:
			message := cat.FailureReason
			if message == "" {
				message = "Category failed"
			}
			issue = &junitIssue{Message: message}
			tc.Failure = issue
			suite.Failures++
		case r.Result == resultFailed:
			message := "Certification failed before the category completed (" + cat.Status + ")"
			if r.FailureReason != "" {
				message += ": " + r.FailureReason
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
			data, err := json.MarshalIndent(cat, "", "  ")
			if err != nil {
				return suite, fmt.Errorf("marshal category report: %w", err)
			}
			issue.Details = string(data)
		}
		suite.Cases = append(suite.Cases, tc)
	}
	if issue, isFailure := certificationIssue(r, suite); issue != nil {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return suite, fmt.Errorf("marshal certification report: %w", err)
		}
		issue.Details = string(data)
		tc := junitCase{Name: "certification", Classname: r.Name}
		if isFailure {
			tc.Failure = issue
			suite.Failures++
		} else {
			tc.Error = issue
			suite.Errors++
		}
		suite.Cases = append(suite.Cases, tc)
	}
	suite.Tests = len(suite.Cases)
	suite.Time = strconv.FormatInt(seconds, 10)
	return suite, nil
}

// certificationIssue returns the one certification-level issue a suite needs
// when its category cases do not already carry the result: a failed
// certification with no failed case (the controller failed it before a
// category could), a running one with no unfinished case (it is finalizing),
// or a certification with no categories at all. isFailure says which element
// it belongs in.
func certificationIssue(r *CertReport, suite junitSuite) (issue *junitIssue, isFailure bool) {
	switch {
	case r.Result == resultFailed && suite.Failures == 0:
		message := r.FailureReason
		if message == "" {
			message = "Certification failed"
		}
		return &junitIssue{Message: message}, true
	case r.Result == resultRunning && suite.Errors == 0:
		return &junitIssue{Message: "Certification did not complete: RUNNING"}, false
	case len(r.Categories) == 0 && r.Result != resultFailed && r.Result != resultRunning:
		return &junitIssue{Message: "Certification reported no categories: " + r.Result}, false
	}
	return nil, false
}

// junitProperties records the certification result, and any targeted nodes
// it left untested, on the suite.
func junitProperties(r *CertReport) []junitProperty {
	props := []junitProperty{{Name: "result", Value: r.Result}}
	if len(r.ExcludedNodes) > 0 {
		props = append(props, junitProperty{Name: "excludedNodes", Value: strings.Join(r.ExcludedNodes, ",")})
	}
	if r.ExclusionReason != "" {
		props = append(props, junitProperty{Name: "exclusionReason", Value: r.ExclusionReason})
	}
	return props
}
