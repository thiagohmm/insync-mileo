package domain

import (
	"testing"
	"time"
)

func TestNullableTime_Scan(t *testing.T) {
	tests := []struct {
		name      string
		input     interface{}
		wantValid bool
		wantTime  string
		wantErr   bool
	}{
		{
			name:      "nil value",
			input:     nil,
			wantValid: false,
			wantTime:  "",
			wantErr:   false,
		},
		{
			name:      "time.Time value",
			input:     time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
			wantValid: true,
			wantTime:  "2024-01-15T10:30:00Z",
			wantErr:   false,
		},
		{
			name:      "byte slice RFC3339",
			input:     []byte("2024-01-15T10:30:00Z"),
			wantValid: true,
			wantTime:  "2024-01-15T10:30:00Z",
			wantErr:   false,
		},
		{
			name:      "string RFC3339",
			input:     "2024-01-15T10:30:00Z",
			wantValid: true,
			wantTime:  "2024-01-15T10:30:00Z",
			wantErr:   false,
		},
		{
			name:      "invalid byte slice",
			input:     []byte("not-a-time"),
			wantValid: false,
			wantErr:   true,
		},
		{
			name:      "invalid string",
			input:     "not-a-time",
			wantValid: false,
			wantErr:   true,
		},
		{
			name:      "unsupported type (int)",
			input:     42,
			wantValid: false,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nt NullableTime
			err := nt.Scan(tt.input)

			if (err != nil) != tt.wantErr {
				t.Errorf("Scan() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if nt.Valid != tt.wantValid {
				t.Errorf("Valid = %v, want %v", nt.Valid, tt.wantValid)
			}

			if tt.wantTime != "" {
				got := nt.Time.UTC().Format(time.RFC3339)
				if got != tt.wantTime {
					t.Errorf("Time = %s, want %s", got, tt.wantTime)
				}
			}
		})
	}
}

func TestNullableTime_Value(t *testing.T) {
	tests := []struct {
		name    string
		nt      NullableTime
		wantVal interface{}
		wantErr bool
	}{
		{
			name:    "invalid returns nil",
			nt:      NullableTime{Valid: false},
			wantVal: nil,
			wantErr: false,
		},
		{
			name:    "valid returns RFC3339 UTC string",
			nt:      NullableTime{Time: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC), Valid: true},
			wantVal: "2024-06-01T12:00:00Z",
			wantErr: false,
		},
		{
			name:    "valid converts local time to UTC",
			nt:      NullableTime{Time: time.Date(2024, 6, 1, 12, 0, 0, 0, time.FixedZone("+0200", 2*3600)), Valid: true},
			wantVal: "2024-06-01T10:00:00Z",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := tt.nt.Value()

			if (err != nil) != tt.wantErr {
				t.Errorf("Value() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if val != tt.wantVal {
				t.Errorf("Value() = %v, want %v", val, tt.wantVal)
			}
		})
	}
}

func TestNullableTime_RoundTrip(t *testing.T) {
	original := NullableTime{
		Time:  time.Date(2024, 3, 15, 14, 30, 45, 0, time.UTC),
		Valid: true,
	}

	// Simulate database round-trip
	val, err := original.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}

	var parsed NullableTime
	if err := parsed.Scan(val); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}

	if !parsed.Valid {
		t.Error("Expected Valid = true after round-trip")
	}

	if !parsed.Time.Equal(original.Time) {
		t.Errorf("Time = %v, want %v", parsed.Time, original.Time)
	}
}
