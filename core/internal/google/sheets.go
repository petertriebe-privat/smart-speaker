package google

import (
	"errors"
	"strconv"
	"strings"

	"echo-assistant/internal/textutil"
	"echo-assistant/internal/tools"
)

// Tables the user keeps with the assistant, as Google Sheets in the Drive folder Jarvis/Tabellen (scope
// drive.file: The assistant sees only the sheets it made). Tools (the admin's account: Private):
// read_sheet (a sheet's rows, or all sheets with links), add_sheet_row (makes the sheet with
// column names when missing), update_sheet_row. As devices/atoms3r/src/services/sheets.cpp.

const (
	sheetsBase     = "https://sheets.googleapis.com/v4/spreadsheets/"
	sheetsFolder   = "Tabellen" // in the assistant's Drive folder
	sheetMIME      = "application/vnd.google-apps.spreadsheet"
	defaultRows    = 20
	maxRows        = 200
	maxSheetTextBy = 8000 // bytes of rows given to the model
)

func sheetLink(id string) string { return "https://docs.google.com/spreadsheets/d/" + id }

func (a *Account) sheets(method, path string, in, out any) error {
	ctx, cancel := callContext()
	defer cancel()
	if method == "GET" {
		return a.get(ctx, sheetsBase+path, out)
	}
	return a.JSON(ctx, method, sheetsBase+path, in, out)
}

// findSheet: the id of the sheet named name in Jarvis/Tabellen, "" if there is none.
func (a *Account) findSheet(name string) (string, error) {
	found, err := a.Drive().List(sheetsFolder, "name="+Quoted(name)+" and mimeType='"+sheetMIME+"'", 1)
	if err != nil || len(found) == 0 {
		return "", err
	}
	return found[0].ID, nil
}

func missingSheet(name string) string {
	return "Error: no sheet \"" + name + "\" (read_sheet with sheet '' lists them)"
}

// rowText: one row as "a | b | c".
func rowText(row []any) string {
	cells := make([]string, len(row))
	for i, cell := range row {
		cells[i] = cellText(cell)
	}
	return strings.Join(cells, " | ")
}

// rowBody: the values of a row for the API: numbers stay numbers, text is read as typed in the
// sheet's locale (USER_ENTERED: dates, formulas starting with =).
func rowBody(values []any) (map[string]any, error) {
	if len(values) == 0 {
		return nil, errors.New("Error: values missing")
	}
	return map[string]any{"values": []any{values}}, nil
}

func (a *Account) readSheet(args map[string]any, _ tools.Context) string {
	name, _ := argString(args, "sheet")
	if name == "" {
		found, err := a.Drive().List(sheetsFolder, "mimeType='"+sheetMIME+"'", 50)
		if err != nil {
			return err.Error()
		}
		if len(found) == 0 {
			return "No sheets yet (add_sheet_row makes one)."
		}
		text := "Sheets (newest change first):\n"
		for _, sheet := range found {
			text += "- " + sheet.Name + ": " + sheetLink(sheet.ID) + "\n"
		}
		return text
	}
	id, err := a.findSheet(name)
	if err != nil {
		return err.Error()
	}
	if id == "" {
		return missingSheet(name)
	}
	var answer map[string]any
	if err := a.sheets("GET", id+"/values/A1:Z5000?majorDimension=ROWS", nil, &answer); err != nil {
		return err.Error()
	}
	rows := digL(answer, "values")
	wanted, ok := tools.Int(args, "last_rows")
	if !ok {
		wanted = defaultRows
	}
	wanted = min(max(wanted, 1), maxRows)
	below := 0
	if len(rows) > 0 {
		below = len(rows) - 1
	}
	text := "Sheet \"" + name + "\" (" + sheetLink(id) + "), " + strconv.Itoa(below) + " rows below the header:\n"
	if len(rows) == 0 {
		return text + "(empty)"
	}
	text += "row 1 (header): " + rowText(digL(rows, 0)) + "\n"
	first := max(1, len(rows)-wanted)
	if first > 1 {
		text += "(rows 2-" + strconv.Itoa(first) + " left out: ask for more with last_rows)\n"
	}
	for i := first; i < len(rows); i++ {
		text += "row " + strconv.Itoa(i+1) + ": " + rowText(digL(rows, i)) + "\n"
	}
	return textutil.Prefix(text, maxSheetTextBy)
}

func (a *Account) addSheetRow(args map[string]any, _ tools.Context) string {
	name := tools.String(args, "sheet")
	if name == "" {
		return "Error: sheet name missing"
	}
	values, _ := args["values"].([]any)
	row, err := rowBody(values)
	if err != nil {
		return err.Error()
	}
	id, err := a.findSheet(name)
	if err != nil {
		return err.Error()
	}
	made := ""
	if id == "" { // a new sheet: header row first
		columns, _ := args["columns"].([]any)
		if len(columns) == 0 {
			return missingSheet(name) + "; to make it, give columns (the header row)"
		}
		if id, err = a.Drive().CreateEmpty(sheetsFolder, name, sheetMIME); err != nil {
			return err.Error()
		}
		header, _ := rowBody(columns)
		if err := a.sheets("PUT", id+"/values/A1?valueInputOption=USER_ENTERED", header, nil); err != nil {
			return err.Error()
		}
		made = "New sheet \"" + name + "\" with the columns " + rowText(columns) + ". "
	}
	var answer map[string]any
	if err := a.sheets("POST", id+"/values/A1:append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS", row, &answer); err != nil {
		return err.Error()
	}
	return made + "Row " + strconv.Itoa(updatedRow(digS(answer, "updates", "updatedRange"))) + " added to \"" + name +
		"\": " + rowText(values) + " (" + sheetLink(id) + ")"
}

// updatedRow: "Tabellenblatt1!A5:C5" -> 5 (0 if there is no number).
func updatedRow(updatedRange string) int {
	for i := strings.IndexByte(updatedRange, '!') + 1; i < len(updatedRange); i++ {
		if updatedRange[i] >= '0' && updatedRange[i] <= '9' {
			j := i
			for j < len(updatedRange) && updatedRange[j] >= '0' && updatedRange[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(updatedRange[i:j])
			return n
		}
	}
	return 0
}

func (a *Account) updateSheetRow(args map[string]any, _ tools.Context) string {
	name, _ := argString(args, "sheet")
	number, _ := tools.Int(args, "row")
	if number < 1 {
		return "Error: row as in read_sheet (1 = header)"
	}
	values, _ := args["values"].([]any)
	row, err := rowBody(values)
	if err != nil {
		return err.Error()
	}
	id, err := a.findSheet(name)
	if err != nil {
		return err.Error()
	}
	if id == "" {
		return missingSheet(name)
	}
	if err := a.sheets("PUT", id+"/values/A"+strconv.Itoa(number)+"?valueInputOption=USER_ENTERED", row, nil); err != nil {
		return err.Error()
	}
	return "Row " + strconv.Itoa(number) + " of \"" + name + "\" is now: " + rowText(values)
}

// AddSheetTools registers read_sheet, add_sheet_row, update_sheet_row and the console test tool
// delete_sheet (the M5 adds them after the agenda tools).
func (a *Account) AddSheetTools(r *tools.Registry) {
	if !a.Enabled() {
		return
	}
	r.Add(tools.Tool{
		Name: "read_sheet",
		Description: "Reads one of the tables the user keeps with the assistant (Google Sheets, e.g. fuel stops, expenses, meter " +
			"readings): the header and the last rows with their row numbers, and the link. sheet '' lists all tables " +
			"with links. Do sums and averages yourself from the rows.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string", "description": "table name, '' = list all"},
                           "last_rows": {"type": "integer", "description": "default 20, at most 200"}},
            "required": ["sheet"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.readSheet})
	r.Add(tools.Tool{
		Name: "add_sheet_row",
		Description: "Adds a row to one of the user's tables (Google Sheets), e.g. 'trag die Tankfüllung ein'. A table that does " +
			"not exist yet is made with columns as its header: choose short German column names and keep the same " +
			"order in values. Look at the header with read_sheet first if unsure. Numbers as JSON numbers, dates as " +
			"DD.MM.YYYY.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string", "description": "table name, e.g. 'Tanken'"},
                           "values": {"type": "array", "items": {"type": ["string", "number"]}},
                           "columns": {"type": "array", "items": {"type": "string"},
                                       "description": "header for a new table, [] if it exists"}},
            "required": ["sheet", "values", "columns"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.addSheetRow})
	r.Add(tools.Tool{
		Name:        "update_sheet_row",
		Description: "Replaces one row of a table (row number from read_sheet), to correct an entry.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string"},
                           "row": {"type": "integer"},
                           "values": {"type": "array", "items": {"type": ["string", "number"]}}},
            "required": ["sheet", "row", "values"]}`,
		Private: true, Channels: tools.Everywhere, Run: a.updateSheetRow})
	// Only for tests over the console: the test table goes to Drive's trash again.
	r.Add(tools.Tool{
		Name:        "delete_sheet",
		Description: "Moves a table to Drive's trash.",
		Parameters: `{"type": "object", "additionalProperties": false,
            "properties": {"sheet": {"type": "string"}}, "required": ["sheet"]}`,
		Channels: tools.Console,
		Run: func(args map[string]any, _ tools.Context) string {
			name, _ := argString(args, "sheet")
			id, err := a.findSheet(name)
			if err != nil {
				return err.Error()
			}
			if id == "" {
				return missingSheet(name)
			}
			if err := a.Drive().TrashFile(id); err != nil {
				return err.Error()
			}
			return "trashed"
		}})
}
