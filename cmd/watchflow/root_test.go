package main

import (
	"bytes"
	"testing"
)

func TestRootCommand(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"--help"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("esperava sucesso ao executar --help, obteve: %v", err)
	}

	output := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte("WatchFlow")) {
		t.Errorf("saída de ajuda não contém 'WatchFlow', obteve:\n%s", output)
	}
}

func TestVersionCommand(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetArgs([]string{"version"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("esperava sucesso ao executar version, obteve: %v", err)
	}
}
