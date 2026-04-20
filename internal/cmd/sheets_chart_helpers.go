package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"google.golang.org/api/sheets/v4"
)

func parseEmbeddedChartJSON(b []byte) (*sheets.EmbeddedChart, error) {
	var chart sheets.EmbeddedChart
	if err := json.Unmarshal(b, &chart); err != nil {
		return nil, err
	}
	if chart.Spec != nil && !chartSpecIsZero(chart.Spec) {
		return &chart, nil
	}

	spec, err := parseChartSpecJSON(b)
	if err != nil {
		return nil, err
	}
	chart.Spec = spec
	return &chart, nil
}

func parseChartSpecJSON(b []byte) (*sheets.ChartSpec, error) {
	var spec sheets.ChartSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, err
	}
	if !chartSpecIsZero(&spec) {
		return &spec, nil
	}

	var chart sheets.EmbeddedChart
	if err := json.Unmarshal(b, &chart); err != nil {
		return nil, err
	}
	if chart.Spec != nil && !chartSpecIsZero(chart.Spec) {
		return chart.Spec, nil
	}
	return nil, usage("--spec-json must contain a ChartSpec or an EmbeddedChart with spec")
}

func chartSpecIsZero(spec *sheets.ChartSpec) bool {
	if spec == nil {
		return true
	}
	return reflect.ValueOf(*spec).IsZero()
}

var gridRangeType = reflect.TypeOf(sheets.GridRange{})

func remapZeroSheetIDsInChartSpec(spec *sheets.ChartSpec, sheetID int64) {
	remapZeroSheetIDs(reflect.ValueOf(spec), sheetID)
}

func remapZeroSheetIDs(v reflect.Value, sheetID int64) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Interface:
		if !v.IsNil() {
			remapZeroSheetIDs(v.Elem(), sheetID)
		}
	case reflect.Ptr:
		if v.IsNil() {
			return
		}
		if v.Type().Elem() == gridRangeType {
			if v.CanInterface() {
				remapGridRange(v.Interface().(*sheets.GridRange), sheetID)
			}
			return
		}
		remapZeroSheetIDs(v.Elem(), sheetID)
	case reflect.Struct:
		if v.Type() == gridRangeType {
			if v.CanAddr() && v.Addr().CanInterface() {
				remapGridRange(v.Addr().Interface().(*sheets.GridRange), sheetID)
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if field.CanSet() || field.Kind() == reflect.Ptr || field.Kind() == reflect.Slice || field.Kind() == reflect.Interface {
				remapZeroSheetIDs(field, sheetID)
			}
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			remapZeroSheetIDs(v.Index(i), sheetID)
		}
	}
}

func remapGridRange(gr *sheets.GridRange, sheetID int64) {
	if gr == nil || gr.SheetId != 0 {
		return
	}
	gr.SheetId = sheetID
	gr.ForceSendFields = appendForceSendField(gr.ForceSendFields, "SheetId")
}

func appendForceSendField(fields []string, field string) []string {
	for _, existing := range fields {
		if existing == field {
			return fields
		}
	}
	return append(fields, field)
}

func firstSheetID(svc *sheets.Service, spreadsheetID string) (int64, error) {
	resp, err := svc.Spreadsheets.Get(spreadsheetID).
		Fields("sheets(properties(sheetId,title))").
		Do()
	if err != nil {
		return 0, err
	}
	for _, sheet := range resp.Sheets {
		if sheet != nil && sheet.Properties != nil {
			return sheet.Properties.SheetId, nil
		}
	}
	return 0, usage("spreadsheet has no sheets")
}

func findChartSheetID(svc *sheets.Service, spreadsheetID string, chartID int64) (int64, error) {
	resp, err := svc.Spreadsheets.Get(spreadsheetID).
		Fields("sheets(properties(sheetId,title),charts(chartId))").
		Do()
	if err != nil {
		return 0, err
	}
	for _, sheet := range resp.Sheets {
		if sheet == nil || sheet.Properties == nil {
			continue
		}
		for _, chart := range sheet.Charts {
			if chart != nil && chart.ChartId == chartID {
				return sheet.Properties.SheetId, nil
			}
		}
	}
	return 0, usagef("chart %d not found", chartID)
}

func resolveChartSheetID(ctx context.Context, svc *sheets.Service, spreadsheetID, sheetName string) (int64, error) {
	if sheetName != "" {
		sheetIDs, err := fetchSheetIDMap(ctx, svc, spreadsheetID)
		if err != nil {
			return 0, err
		}
		id, ok := sheetIDs[sheetName]
		if !ok {
			return 0, usagef("unknown sheet %q", sheetName)
		}
		return id, nil
	}
	return firstSheetID(svc, spreadsheetID)
}

func buildChartPosition(sheetID int64, anchor string, width, height int64) (*sheets.EmbeddedObjectPosition, error) {
	var rowIndex, colIndex int64
	if anchor != "" {
		parsed, err := parseA1Cell(anchor)
		if err != nil {
			return nil, fmt.Errorf("invalid --anchor %q: %w", anchor, err)
		}
		rowIndex = int64(parsed.row - 1)
		colIndex = int64(parsed.col - 1)
	}

	return &sheets.EmbeddedObjectPosition{
		OverlayPosition: &sheets.OverlayPosition{
			AnchorCell: &sheets.GridCoordinate{
				SheetId:         sheetID,
				RowIndex:        rowIndex,
				ColumnIndex:     colIndex,
				ForceSendFields: []string{"SheetId", "RowIndex", "ColumnIndex"},
			},
			WidthPixels:  width,
			HeightPixels: height,
		},
	}, nil
}

type a1Cell struct {
	row int
	col int
}

func parseA1Cell(cell string) (a1Cell, error) {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return a1Cell{}, fmt.Errorf("empty cell reference")
	}

	i := 0
	for i < len(cell) && ((cell[i] >= 'A' && cell[i] <= 'Z') || (cell[i] >= 'a' && cell[i] <= 'z')) {
		i++
	}
	if i == 0 || i == len(cell) {
		return a1Cell{}, fmt.Errorf("invalid cell reference %q", cell)
	}

	col, err := colLettersToIndex(strings.ToUpper(cell[:i]))
	if err != nil {
		return a1Cell{}, err
	}
	row, err := strconv.Atoi(cell[i:])
	if err != nil || row < 1 {
		return a1Cell{}, fmt.Errorf("invalid row in cell reference %q", cell)
	}
	return a1Cell{row: row, col: col}, nil
}
