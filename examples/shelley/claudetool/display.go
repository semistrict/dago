package claudetool

// BashDisplayData is retained as Shelley's UI projection for historical and
// deterministic-fixture command results. Production command execution is owned
// by dago's execute tool.
type BashDisplayData struct {
	WorkingDir string `json:"workingDir"`
}

// PatchDisplayData is retained for persisted conversations and debug fixtures.
// Production file edits are owned by dago's edit_file and write_file tools.
type PatchDisplayData struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}
