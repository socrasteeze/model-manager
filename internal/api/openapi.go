package api

import (
	"encoding/json"
	"fmt"
)

// openAPISpec describes the API (spec §11: "REST, documented with OpenAPI").
//
// Written by hand rather than generated. The API is meant to be a stable
// interop surface that third-party front-ends depend on, and a hand-written
// document is one that says what is guaranteed rather than whatever the current
// structs happen to serialize.
func openAPISpec(version string) []byte {
	if version == "" {
		version = "dev"
	}

	spec := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "Model Manager API",
			"version": version,
			"description": "Content-addressed metadata for a local AI model library. " +
				"Models are keyed by the SHA256 of the file; paths are mutable attributes.",
		},
		"servers": []any{
			map[string]any{"url": "/", "description": "This daemon"},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearer": map[string]any{
					"type":   "http",
					"scheme": "bearer",
					"description": "Required when the daemon is bound to a non-loopback " +
						"interface. The token is written to a file beside the database for " +
						"CLI and third-party use, and injected into the bundled UI.",
				},
			},
			"schemas": map[string]any{
				"Error": object(map[string]any{
					"error":  str("Short machine-readable message"),
					"detail": str("Human-readable detail"),
				}, "error"),

				"SearchHit": object(map[string]any{
					"sha256":        str("Primary key: SHA256 of the file"),
					"name":          str(""),
					"type":          str("checkpoint | lora | lycoris | vae | embedding | controlnet | upscaler"),
					"base_model":    str(""),
					"version":       str(""),
					"origin":        str("civitai | huggingface | self-trained | unknown"),
					"format":        str("safetensors | gguf | ckpt | pt"),
					"size":          integer("Bytes"),
					"trigger_words": array(str("")),
					"tags":          array(str("")),
					"preview_image": str("Content address; fetch from /api/previews/{image}"),
					"filename":      str(""),
					"path":          str("One known path; see the detail endpoint for all"),
					"path_count":    integer("Number of present paths"),
					"present":       boolean("Whether any path is currently on disk"),
					"nsfw":          boolean(""),
				}, "sha256", "format", "size", "present"),

				"SearchResults": object(map[string]any{
					"hits":   array(ref("SearchHit")),
					"total":  integer("Matches before paging"),
					"limit":  integer(""),
					"offset": integer(""),
				}, "hits", "total"),

				"ModelRecord": object(map[string]any{
					"sha256":               str(""),
					"type":                 str(""),
					"base_model":           str(""),
					"name":                 str(""),
					"version":              str(""),
					"description":          str(""),
					"trigger_words":        array(str("")),
					"recommended_weight":   number(""),
					"recommended_settings": str("JSON object, encoded as a string"),
					"nsfw":                 boolean(""),
					"origin":               str(""),
					"updated_at":           str(""),
				}, "sha256"),

				"FilePath": object(map[string]any{
					"ID":          integer(""),
					"SHA256":      str(""),
					"Path":        str(""),
					"Root":        str(""),
					"Present":     boolean("False means seen before but not in the latest scan"),
					"Provisional": boolean("Bound by sampled probe; not yet confirmed by a full hash"),
					"Size":        integer(""),
				}, "Path", "Present"),

				"TrainingRecord": object(map[string]any{
					"sha256":       str(""),
					"dataset":      str(""),
					"dataset_size": integer("Number of training images"),
					"base":         str(""),
					"config":       object(nil),
					"trainer":      str("ai-toolkit | Anima TrainFlow | OneTrainer | kohya-ss"),
					"notes":        str("What worked and what did not"),
					"run_date":     str(""),
					"source":       str("manual | safetensors_header"),
				}, "sha256", "source"),

				"Suggestion": object(map[string]any{
					"id":              integer(""),
					"sha256":          str(""),
					"field":           str(""),
					"manual_value":    str("JSON-encoded"),
					"suggested_value": str("JSON-encoded"),
					"source":          str(""),
					"status":          str("pending | accepted | dismissed"),
					"created_at":      str(""),
				}, "id", "field"),

				"ModelDetail": object(map[string]any{
					"sha256":         str(""),
					"weights_sha256": str("Tensor-region hash; absent for .ckpt/.pt and unparseable framing"),
					"size":           integer(""),
					"format":         str(""),
					"first_seen":     str(""),
					"last_verified":  str(""),
					"record":         ref("ModelRecord"),
					"paths":          array(ref("FilePath")),
					"previews":       array(object(nil)),
					"tags":           array(str("")),
					"training":       ref("TrainingRecord"),
					"suggestions":    array(ref("Suggestion")),
				}, "sha256", "size", "format"),
			},
		},
		"security": []any{map[string]any{"bearer": []any{}}},
		"paths": map[string]any{
			"/api/health": get("Liveness and build information", "Health", jsonResponse(object(nil))),

			"/api/models": get("Search the library", "Models",
				jsonResponse(ref("SearchResults")),
				queryParam("q", "Full-text query over name, description, trigger words, tags and filename"),
				queryParam("type", "Filter by model type; repeatable or comma-separated"),
				queryParam("base_model", "Filter by base model; repeatable"),
				queryParam("tag", "Filter by tag; repeatable. All listed tags must be present"),
				queryParam("origin", "Filter by origin"),
				queryParam("format", "Filter by container format"),
				queryParam("has_preview", "true or false"),
				queryParam("nsfw", "true or false"),
				queryParam("present", "true to restrict to files currently on disk"),
				queryParam("needs_attention", "true to list records missing a name, base model or preview"),
				queryParam("sort", "name | size | recent | added"),
				queryParam("order", "asc or desc"),
				queryParam("limit", "Page size, max 500"),
				queryParam("offset", "Page offset"),
			),

			"/api/models/{sha}": map[string]any{
				"get": operation("Everything known about one model", "Models",
					jsonResponse(ref("ModelDetail")), pathParam("sha", "SHA256 of the file")),
				"patch": operation("Set manual field values", "Models",
					jsonResponse(ref("ModelRecord")), pathParam("sha", "SHA256 of the file")),
			},

			"/api/models/{sha}/fields/{field}": map[string]any{
				"delete": operation(
					"Clear a manual value so lower-tier sources resolve again", "Models",
					jsonResponse(ref("ModelRecord")),
					pathParam("sha", "SHA256 of the file"),
					pathParam("field", "Field name")),
			},

			"/api/models/{sha}/candidates": get(
				"Every stored opinion about each field, winner first", "Provenance",
				jsonResponse(array(object(nil))), pathParam("sha", "SHA256 of the file")),

			"/api/models/{sha}/tags": map[string]any{
				"put": operation("Replace the manual tag set", "Models",
					jsonResponse(array(str(""))), pathParam("sha", "SHA256 of the file")),
			},

			"/api/models/{sha}/training": map[string]any{
				"get": operation("Read the training record", "Training",
					jsonResponse(ref("TrainingRecord")), pathParam("sha", "SHA256 of the file")),
				"put": operation("Write the training record (always recorded as manual)", "Training",
					jsonResponse(ref("TrainingRecord")), pathParam("sha", "SHA256 of the file")),
			},

			"/api/suggestions": get(
				"Pending disagreements between a manual value and an origin", "Provenance",
				jsonResponse(array(ref("Suggestion"))),
				queryParam("sha256", "Restrict to one model"),
				queryParam("limit", "Maximum returned")),

			"/api/suggestions/{id}/accept": map[string]any{
				"post": operation("Adopt the suggested value as the manual value", "Provenance",
					emptyResponse(), pathParam("id", "Suggestion id")),
			},
			"/api/suggestions/{id}/dismiss": map[string]any{
				"post": operation("Keep the manual value and stop asking", "Provenance",
					emptyResponse(), pathParam("id", "Suggestion id")),
			},

			"/api/facets": get("Distinct filter values with counts", "Discovery",
				jsonResponse(object(nil))),
			"/api/tags": get("Every tag with a usage count", "Discovery",
				jsonResponse(object(nil))),
			"/api/stats": get("Library totals, duplication and size distribution", "Discovery",
				jsonResponse(object(nil)), queryParam("top", "Duplicate groups to include")),
			"/api/scans": get("Recent scan runs", "Discovery",
				jsonResponse(array(object(nil))), queryParam("limit", "Maximum returned")),
			"/api/detect": get("Detected SD tool installations and their model roots", "Discovery",
				jsonResponse(object(nil))),

			"/api/previews/{image}": get("Fetch a preview image by content address", "Media",
				map[string]any{
					"200": map[string]any{
						"description": "Image bytes",
						"content": map[string]any{
							"image/*": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
						},
					},
					"404": jsonRef("Not found", "Error"),
				},
				pathParam("image", "SHA256 of the image itself")),
		},
	}

	out, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":"failed to render OpenAPI: %s"}`, err))
	}
	return out
}

// --- small builders to keep the document above readable ------------------------

func str(desc string) map[string]any     { return typed("string", desc) }
func integer(desc string) map[string]any { return typed("integer", desc) }
func number(desc string) map[string]any  { return typed("number", desc) }
func boolean(desc string) map[string]any { return typed("boolean", desc) }

func typed(t, desc string) map[string]any {
	m := map[string]any{"type": t}
	if desc != "" {
		m["description"] = desc
	}
	return m
}

func array(items any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func object(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object"}
	if props != nil {
		m["properties"] = props
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func ref(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func jsonResponse(schema any) map[string]any {
	return map[string]any{
		"200": map[string]any{
			"description": "Success",
			"content":     map[string]any{"application/json": map[string]any{"schema": schema}},
		},
		"401": jsonRef("A bearer token is required", "Error"),
		"403": jsonRef("Host or Origin rejected, or the server is read-only", "Error"),
	}
}

func emptyResponse() map[string]any {
	return map[string]any{
		"204": map[string]any{"description": "Done"},
		"401": jsonRef("A bearer token is required", "Error"),
	}
}

func jsonRef(description, schema string) map[string]any {
	return map[string]any{
		"description": description,
		"content":     map[string]any{"application/json": map[string]any{"schema": ref(schema)}},
	}
}

func get(summary, tag string, responses map[string]any, params ...map[string]any) map[string]any {
	return map[string]any{"get": operation(summary, tag, responses, params...)}
}

func operation(summary, tag string, responses map[string]any, params ...map[string]any) map[string]any {
	op := map[string]any{
		"summary":   summary,
		"tags":      []any{tag},
		"responses": responses,
	}
	if len(params) > 0 {
		list := make([]any, len(params))
		for i, p := range params {
			list[i] = p
		}
		op["parameters"] = list
	}
	return op
}

func queryParam(name, desc string) map[string]any {
	return map[string]any{
		"name": name, "in": "query", "description": desc,
		"schema": map[string]any{"type": "string"},
	}
}

func pathParam(name, desc string) map[string]any {
	return map[string]any{
		"name": name, "in": "path", "required": true, "description": desc,
		"schema": map[string]any{"type": "string"},
	}
}
