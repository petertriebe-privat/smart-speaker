#pragma once

// Tables the user keeps with the assistant, as Google Sheets in the Drive folder
// The assistant/Tabellen (scope drive.file: The assistant sees only the sheets it made). Tools (slow, the
// admin's account: Private): read_sheet (a sheet's rows, or all sheets with links),
// add_sheet_row (makes the sheet with column names when missing), update_sheet_row.
namespace sheets {

void addTools();

}  // namespace sheets
