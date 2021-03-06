// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Radu Berinde (radu@cockroachlabs.com)

package distsql

import (
	"sync"
	"testing"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/sql/sqlbase"
	"github.com/cockroachdb/cockroach/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/util/leaktest"
)

func TestJoinReader(t *testing.T) {
	defer leaktest.AfterTest(t)()

	_, sqlDB, kvDB, cleanup := sqlutils.SetupServer(t)
	defer cleanup()

	// Create a table where each row is:
	//
	//  |     a    |     b    |         sum         |         s           |
	//  |-----------------------------------------------------------------|
	//  | rowId/10 | rowId%10 | rowId/10 + rowId%10 | IntToEnglish(rowId) |

	aFn := func(row int) parser.Datum {
		return parser.NewDInt(parser.DInt(row / 10))
	}
	bFn := func(row int) parser.Datum {
		return parser.NewDInt(parser.DInt(row % 10))
	}
	sumFn := func(row int) parser.Datum {
		return parser.NewDInt(parser.DInt(row/10 + row%10))
	}

	sqlutils.CreateTable(t, sqlDB, "t",
		"a INT, b INT, sum INT, s STRING, PRIMARY KEY (a,b), INDEX bs (b,s)",
		99,
		sqlutils.ToRowFn(aFn, bFn, sumFn, sqlutils.RowEnglishFn))

	td := sqlbase.GetTableDescriptor(kvDB, "test", "t")

	testCases := []struct {
		spec     JoinReaderSpec
		input    [][]parser.Datum
		expected string
	}{
		{
			spec: JoinReaderSpec{
				OutputColumns: []uint32{0, 1, 2},
			},
			input: [][]parser.Datum{
				{aFn(2), bFn(2)},
				{aFn(5), bFn(5)},
				{aFn(10), bFn(10)},
				{aFn(15), bFn(15)},
			},
			expected: "[[0 2 2] [0 5 5] [1 0 1] [1 5 6]]",
		},
		{
			spec: JoinReaderSpec{
				Filter:        Expression{Expr: "$2 <= 5"}, // sum <= 5
				OutputColumns: []uint32{3},
			},
			input: [][]parser.Datum{
				{aFn(1), bFn(1)},
				{aFn(25), bFn(25)},
				{aFn(5), bFn(5)},
				{aFn(21), bFn(21)},
				{aFn(34), bFn(34)},
				{aFn(13), bFn(13)},
				{aFn(51), bFn(51)},
				{aFn(50), bFn(50)},
			},
			expected: "[['one'] ['five'] ['two-one'] ['one-three'] ['five-zero']]",
		},
	}
	for _, c := range testCases {
		js := c.spec
		js.Table = *td

		txn := client.NewTxn(context.Background(), *kvDB)

		out := &testingReceiver{}
		jr, err := newJoinReader(&js, txn, out, &parser.EvalContext{})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go jr.Run(&wg)
		for _, row := range c.input {
			encRow := make(sqlbase.EncDatumRow, len(row))
			for i, d := range row {
				encRow[i].SetDatum(sqlbase.ColumnType_INT, d)
			}
			ok := jr.PushRow(encRow)
			if !ok {
				t.Fatal("joinReader stopped accepting rows")
			}
		}
		jr.Close(nil)
		wg.Wait()
		if out.err != nil {
			t.Fatal(out.err)
		}
		if !out.closed {
			t.Fatalf("output rowReceiver not closed")
		}
		if result := out.rows.String(); result != c.expected {
			t.Errorf("invalid results: %s, expected %s'", result, c.expected)
		}
	}
}
