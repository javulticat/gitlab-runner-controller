package state

import (
	"encoding/json"
	"testing"
)

func TestOpenRejectsRetiredAndInvalidJobFieldsWithoutRewriting(t *testing.T) {
	for name, job := range map[string]string{
		"ref":           `{"project_id":1,"id":7,"ref":"v1"}`,
		"tag":           `{"project_id":1,"id":7,"tag":true}`,
		"pipeline-id":   `{"project_id":1,"id":7,"pipeline_id":100}`,
		"unknown":       `{"project_id":1,"id":7,"unexpected":1}`,
		"runner-type":   `{"project_id":1,"id":7,"runner_id":"42"}`,
		"null":          `null`,
		"array":         `[]`,
		"missing-ids":   `{}`,
		"negative-id":   `{"project_id":1,"id":-7}`,
		"mismatched-id": `{"project_id":1,"id":8}`,
	} {
		t.Run(name, func(t *testing.T) {
			saved := New("test", "gitlab/1")
			data, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			fields["jobs"] = json.RawMessage(`{"1/7":` + job + `}`)
			data, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			assertRejectedStateFile(t, data)
		})
	}
}
