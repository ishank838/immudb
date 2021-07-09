/*
Copyright 2021 CodeNotary, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"errors"
	"github.com/codenotary/immudb/embedded/sql"
	"github.com/codenotary/immudb/pkg/api/schema"
	bm "github.com/codenotary/immudb/pkg/pgsql/server/bmessages"
	fm "github.com/codenotary/immudb/pkg/pgsql/server/fmessages"
	"github.com/codenotary/immudb/pkg/pgsql/server/pgmeta"
	"io"
	"regexp"
	"sort"
	"strings"
)

func (s *session) QueryMachine() (err error) {
	s.Lock()
	defer s.Unlock()

	var portals = make(map[string]*portal)
	var statements = make(map[string]*statement)

	var waitForSync = false

	if _, err := s.writeMessage(bm.ReadyForQuery()); err != nil {
		return err
	}

	for {
		msg, extQueryMode, err := s.nextMessage()
		if err != nil {
			if err == io.EOF {
				s.log.Warningf("connection is closed")
				return nil
			}
			s.ErrorHandle(err)
			if extQueryMode {
				waitForSync = true
			}
			continue
		}
		// When an error is detected while processing any extended-query message, the backend issues ErrorResponse,
		// then reads and discards messages until a Sync is reached, then issues ReadyForQuery and returns to normal
		// message processing. (But note that no skipping occurs if an error is detected while processing Sync — this
		// ensures that there is one and only one ReadyForQuery sent for each Sync.)
		if waitForSync && extQueryMode {
			if _, ok := msg.(fm.SyncMsg); !ok {
				continue
			}
			waitForSync = false
			extQueryMode = false
		}

		switch v := msg.(type) {
		case fm.TerminateMsg:
			return s.mr.CloseConnection()
		case fm.QueryMsg:
			var set = regexp.MustCompile(`(?i)set\s+.+`)
			if set.MatchString(v.GetStatements()) {
				if _, err := s.writeMessage(bm.CommandComplete([]byte(`ok`))); err != nil {
					s.ErrorHandle(err)
				}
				if _, err := s.writeMessage(bm.ReadyForQuery()); err != nil {
					s.ErrorHandle(err)
					continue
				}
				continue
			}
			var version = regexp.MustCompile(`(?i)select\s+version\(\s*\)`)
			if version.MatchString(v.GetStatements()) {
				if err = s.writeVersionInfo(); err != nil {
					s.ErrorHandle(err)
					continue
				}
				if _, err := s.writeMessage(bm.ReadyForQuery()); err != nil {
					s.ErrorHandle(err)
					continue
				}
				continue
			}
			if v.GetStatements() == ";" {
				if _, err := s.writeMessage(bm.CommandComplete([]byte(`ok`))); err != nil {
					s.ErrorHandle(err)
				}
				if _, err := s.writeMessage(bm.ReadyForQuery()); err != nil {
					s.ErrorHandle(err)
					continue
				}
				continue
			}
			// todo handle the result outside in order to avoid err suppression
			if _, err = s.queryMsg(v.GetStatements()); err != nil {
				s.ErrorHandle(err)
				continue
			}
			if _, err := s.writeMessage(bm.CommandComplete([]byte(`ok`))); err != nil {
				s.ErrorHandle(err)
				continue
			}
			if _, err := s.writeMessage(bm.ReadyForQuery()); err != nil {
				s.ErrorHandle(err)
				continue
			}
		case fm.ParseMsg:
			var set = regexp.MustCompile(`(?i)set\s+.+`)
			var paramCols []*schema.Column
			var resCols []*schema.Column
			var stmt sql.SQLStmt
			if !set.MatchString(v.Statements) {
				// todo @Michele The query string contained in a Parse message cannot include more than one SQL statement;
				// else a syntax error is reported. This restriction does not exist in the simple-query protocol,
				// but it does exist in the extended protocol, because allowing prepared statements or portals to contain
				// multiple commands would complicate the protocol unduly.
				stmts, err := sql.Parse(strings.NewReader(v.Statements))
				if err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}
				if len(stmts) > 1 {
					s.ErrorHandle(ErrMaxStmtNumberExceeded)
					continue
				}
				stmt = stmts[0]

				sel, ok := stmt.(*sql.SelectStmt)
				if ok != true {
					s.ErrorHandle(errors.New("not a select statement"))
					waitForSync = true
					continue
				}
				rr, err := s.database.SQLQueryRowReader(sel, true)
				if err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}
				cols, err := rr.Columns()
				if err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}
				resCols = make([]*schema.Column, 0)
				for _, c := range cols {
					resCols = append(resCols, &schema.Column{Name: c.Selector, Type: c.Type})
				}

				r, err := s.database.InferParametersPrepared(stmt)
				if err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}

				paramsNameList := []string{}
				for n, _ := range r {
					paramsNameList = append(paramsNameList, n)
				}
				sort.Strings(paramsNameList)

				paramCols = make([]*schema.Column, 0)
				for _, n := range paramsNameList {
					paramCols = append(paramCols, &schema.Column{Name: n, Type: r[n]})
				}
			}
			_, ok := statements[v.DestPreparedStatementName]
			// unnamed prepared statement overrides previous
			if ok && v.DestPreparedStatementName != "" {
				return errors.New("statement already present")
			}

			newStatement := &statement{
				// if no name is provided empty string marks the unnamed prepared statement
				Name:         v.DestPreparedStatementName,
				Params:       paramCols,
				SQLStatement: v.Statements,
				PreparedStmt: stmt,
				Results:      resCols,
			}

			statements[v.DestPreparedStatementName] = newStatement

			if _, err = s.writeMessage(bm.ParseComplete()); err != nil {
				s.ErrorHandle(err)
				waitForSync = true
				continue
			}
		case fm.DescribeMsg:

			// The Describe message (statement variant) specifies the name of an existing prepared statement
			// (or an empty string for the unnamed prepared statement). The response is a ParameterDescription
			// message describing the parameters needed by the statement, followed by a RowDescription message
			// describing the rows that will be returned when the statement is eventually executed (or a NoData
			// message if the statement will not return rows). ErrorResponse is issued if there is no such prepared
			// statement. Note that since Bind has not yet been issued, the formats to be used for returned columns
			// are not yet known to the backend; the format code fields in the RowDescription message will be zeroes
			// in this case.
			if v.DescType == "S" {
				st, ok := statements[v.Name]
				if !ok {
					s.ErrorHandle(errors.New("statement not found"))
					waitForSync = true
					continue
				}
				if _, err = s.writeMessage(bm.ParameterDescription(st.Params)); err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}
				if _, err := s.writeMessage(bm.RowDescription(st.Results, nil)); err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}
			}
			// The Describe message (portal variant) specifies the name of an existing portal (or an empty string
			// for the unnamed portal). The response is a RowDescription message describing the rows that will be
			// returned by executing the portal; or a NoData message if the portal does not contain a query that
			// will return rows; or ErrorResponse if there is no such portal.
			if v.DescType == "P" {
				st, ok := portals[v.Name]
				if !ok {
					s.ErrorHandle(errors.New("portal not found"))
					waitForSync = true
					continue
				}
				if _, err := s.writeMessage(bm.RowDescription(st.Statement.Results, st.ResultColumnFormatCodes)); err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}
			}

		// sync
		case fm.SyncMsg:
			if _, err := s.writeMessage(bm.ReadyForQuery()); err != nil {
				s.ErrorHandle(err)
			}
		case fm.BindMsg:
			_, ok := portals[v.DestPortalName]
			// unnamed portal overrides previous
			if ok && v.DestPortalName != "" {
				s.ErrorHandle(errors.New("portal already present"))
				waitForSync = true
				continue
			}

			st, ok := statements[v.PreparedStatementName]
			if !ok {
				s.ErrorHandle(errors.New("statement not found"))
				waitForSync = true
				continue
			}

			encodedParams, err := buildNamedParams(st.Params, v.ParamVals)
			if err != nil {
				s.ErrorHandle(err)
				waitForSync = true
				continue
			}

			newPortal := &portal{
				Name:                    v.DestPortalName,
				Statement:               st,
				Parameters:              encodedParams,
				ResultColumnFormatCodes: v.ResultColumnFormatCodes,
			}
			portals[v.DestPortalName] = newPortal

			if _, err := s.writeMessage(bm.BindComplete()); err != nil {
				s.ErrorHandle(err)
				waitForSync = true
				continue
			}
		case fm.Execute:
			var set = regexp.MustCompile(`(?i)set\s+.+`)
			if set.MatchString(portals[v.PortalName].Statement.SQLStatement) {
				if _, err = s.writeMessage(bm.EmptyQueryResponse()); err != nil {
					s.ErrorHandle(err)
					waitForSync = true
					continue
				}
				continue
			}
			//query execution
			stmts, err := sql.Parse(strings.NewReader(portals[v.PortalName].Statement.SQLStatement))
			if err != nil {
				s.ErrorHandle(err)
				waitForSync = true
				continue
			}

			for _, stmt := range stmts {
				switch st := stmt.(type) {
				case *sql.SelectStmt:
					res, err := s.database.SQLQueryPrepared(st, portals[v.PortalName].Parameters, true)
					if err != nil {
						s.ErrorHandle(err)
						waitForSync = true
						continue
					}
					if res != nil && len(res.Rows) > 0 {
						if _, err = s.writeMessage(bm.DataRow(res.Rows, len(res.Columns), portals[v.PortalName].ResultColumnFormatCodes)); err != nil {
							s.ErrorHandle(err)
							waitForSync = true
							continue
						}
						break
					}
					if _, err = s.writeMessage(bm.EmptyQueryResponse()); err != nil {
						s.ErrorHandle(err)
						waitForSync = true
						continue
					}
				}
			}
			if _, err := s.writeMessage(bm.CommandComplete([]byte(`ok`))); err != nil {
				s.ErrorHandle(err)
				waitForSync = true
				continue
			}
		default:
			s.ErrorHandle(ErrUnknowMessageType)
			continue
		}
	}
}

func (s *session) queryMsg(statements string) (*schema.SQLExecResult, error) {
	var res *schema.SQLExecResult
	stmts, err := sql.Parse(strings.NewReader(statements))
	if err != nil {
		return nil, err
	}
	for _, stmt := range stmts {
		switch st := stmt.(type) {
		case *sql.UseDatabaseStmt:
			{
				return nil, ErrUseDBStatementNotSupported
			}
		case *sql.CreateDatabaseStmt:
			{
				return nil, ErrCreateDBStatementNotSupported
			}
		case *sql.SelectStmt:
			err := s.selectStatement(st)
			if err != nil {
				return nil, err
			}
		case sql.SQLStmt:
			res, err = s.database.SQLExecPrepared([]sql.SQLStmt{st}, nil, true)
			if err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

func (s *session) selectStatement(st *sql.SelectStmt) error {
	res, err := s.database.SQLQueryPrepared(st, nil, true)
	if err != nil {
		return err
	}
	if res != nil && len(res.Rows) > 0 {
		if _, err = s.writeMessage(bm.RowDescription(res.Columns, nil)); err != nil {
			return err
		}
		if _, err = s.writeMessage(bm.DataRow(res.Rows, len(res.Columns), nil)); err != nil {
			return err
		}
		return nil
	}
	if _, err = s.writeMessage(bm.EmptyQueryResponse()); err != nil {
		return err
	}
	return nil
}

func (s *session) writeVersionInfo() error {
	cols := []*schema.Column{{Name: "version", Type: "VARCHAR"}}
	if _, err := s.writeMessage(bm.RowDescription(cols, nil)); err != nil {
		return err
	}
	rows := []*schema.Row{{
		Columns: []string{"version"},
		Values:  []*schema.SQLValue{{Value: &schema.SQLValue_S{S: pgmeta.PgsqlProtocolVersionMessage}}},
	}}
	if _, err := s.writeMessage(bm.DataRow(rows, len(cols), nil)); err != nil {
		return err
	}
	if _, err := s.writeMessage(bm.CommandComplete([]byte(`ok`))); err != nil {
		return err
	}
	if _, err := s.writeMessage(bm.ReadyForQuery()); err != nil {
		return err
	}
	return nil
}

type portal struct {
	Name                    string
	Statement               *statement
	Parameters              []*schema.NamedParam
	ResultColumnFormatCodes []int16
}

type statement struct {
	Name         string
	SQLStatement string
	PreparedStmt sql.SQLStmt
	Params       []*schema.Column
	Results      []*schema.Column
}
