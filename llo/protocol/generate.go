package protocol

//go:generate protoc --go_out=. --go_opt=paths=source_relative attested_retirement_report.proto
//go:generate protoc --go_out=. --go_opt=paths=source_relative llo_offchain_config.proto
//go:generate protoc --go_out=. --go_opt=paths=source_relative llo_plugin_telemetry.proto
//go:generate protoc --go_out=. --go_opt=paths=source_relative plugin_codecs.proto
