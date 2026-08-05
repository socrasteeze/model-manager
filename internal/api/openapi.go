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

			// Managed roots. Every root is also a legal download destination,
			// so adding one is the operation that widens where this daemon may
			// write -- guarded like the download endpoint, and refused outright
			// when it would overlap a root already managed.
			"/api/roots": map[string]any{
				"get": operation("Managed model directories with file counts", "Roots",
					jsonResponse(object(nil))),
				"post": operation("Add a directory (canonicalized; overlap refused)", "Roots",
					jsonResponse(object(nil))),
			},
			"/api/roots/{id}": map[string]any{
				"patch": operation("Enable, disable, label or set the folder layout", "Roots",
					jsonResponse(object(nil)), pathParam("id", "Root id")),
				"delete": operation(
					"Forget a directory. Nothing on disk is touched: the paths are marked "+
						"absent and the model records and metadata are kept, so re-adding "+
						"the folder restores the library.",
					"Roots", jsonResponse(object(nil)), pathParam("id", "Root id")),
			},
			"/api/scans/active": get("Progress of the running or most recent scan", "Roots",
				jsonResponse(object(nil))),
			"/api/scans/{id}": map[string]any{
				"delete": operation("Cancel the running scan", "Roots",
					jsonResponse(object(nil)), pathParam("id", "Scan id")),
			},

			"/api/settings": get("Every stored preference", "Settings",
				jsonResponse(object(nil))),
			"/api/settings/{key}": map[string]any{
				"put": operation("Store a preference (JSON value)", "Settings",
					jsonResponse(object(nil)), pathParam("key", "Setting key")),
				"delete": operation("Reset a preference to its built-in default", "Settings",
					jsonResponse(object(nil)), pathParam("key", "Setting key")),
			},

			"/api/downloads/destination": get(
				"Where a download of this type would land under this root. Resolved "+
					"server-side because the subfolder depends on which tool's vocabulary "+
					"the root uses -- the same lora is Lora under Stability Matrix and "+
					"loras under ComfyUI.",
				"Downloads", jsonResponse(object(nil)),
				queryParam("root", "Destination root (defaults to the configured one)"),
				queryParam("type", "Model type as the provider spells it")),
			"/api/downloads/folder-defaults": get(
				"Built-in per-tool folder vocabularies", "Downloads", jsonResponse(object(nil))),

			"/api/generated": get(
				"Recent images under the configured ComfyUI output folder", "Media",
				jsonResponse(object(nil)), queryParam("limit", "Maximum returned")),

			"/api/models/{sha}/previews": map[string]any{
				"post": operation(
					"Attach an image as this model's thumbnail. The body is raw image "+
						"bytes; the type is sniffed rather than taken from a header. Stored "+
						"with a manual source, which outranks every fetched preview, so "+
						"enrichment can never displace a chosen thumbnail.",
					"Media", jsonResponse(object(nil)), pathParam("sha", "Model SHA256")),
			},
			"/api/models/{sha}/previews/{image}": map[string]any{
				"delete": operation(
					"Detach an image. The blob is kept: blobs are content-addressed and "+
						"shared, so deleting the bytes would blank other models too.",
					"Media", emptyResponse(),
					pathParam("sha", "Model SHA256"), pathParam("image", "Image SHA256")),
			},
			// --- remote browsing (Phase 6) --------------------------------
			"/api/browse": get(
				"Search Civitai, CivArchive and HuggingFace, marking every result "+
					"have / update / new by content hash rather than by filename",
				"Browse", jsonResponse(object(nil)),
				queryParam("q", "Search text"),
				queryParam("provider", "Restrict to one provider (repeatable)"),
				queryParam("type", "Model type"),
				queryParam("base_model", "Base-model family"),
				queryParam("sort", "Provider sort order"),
				queryParam("page", "Page number"),
				queryParam("nsfw", "Permit adult results")),
			// Reading is separate from checking on purpose. The GET used to
			// perform the whole sweep inline and throw the answer away when the
			// tab closed; now it reads what the last sweep recorded, so the
			// result survives a restart and can be filtered on.
			"/api/updates": map[string]any{
				"get": operation(
					"Models held for which the origin publishes a newer version, as of "+
						"the last check. Reads stored data -- no network, and available "+
						"even on a --no-remote daemon, which can still show what an "+
						"earlier check found.",
					"Updates", jsonResponse(object(nil)),
					queryParam("limit", "Maximum returned")),
				"post": operation(
					"Start a background update sweep: one throttled request per owned "+
						"model. Refused with 409 while an enrichment sweep is running, "+
						"since both spend the same provider rate limit.",
					"Updates", jsonResponse(object(nil)),
					queryParam("limit", "Check at most this many models (0 for all)"),
					queryParam("max_age_hours", "Skip models checked more recently than this")),
			},
			"/api/updates/{id}": map[string]any{
				"delete": operation(
					"Stop the running sweep. Everything already recorded stays "+
						"recorded, and re-running continues where this left off.",
					"Updates", emptyResponse(), pathParam("id", "Update run id")),
			},

			// --- enrichment ------------------------------------------------
			// Everything fetched is recorded as an ordinary origin-tier
			// observation and resolved by the usual rules, so a manual value
			// still wins and a manual preview still cannot be displaced. These
			// endpoints add a trigger, not a new precedence.
			"/api/models/{sha}/enrich": map[string]any{
				"post": operation(
					"Look this model up at the origin by hash and merge what comes back. "+
						"Refuses with 409 when the hash is provisional: a hash bound by "+
						"sampled probe could archive another file's metadata here.",
					"Enrichment", jsonResponse(object(nil)),
					pathParam("sha", "Model SHA256"),
					queryParam("refresh", "Re-ask even if a response is already archived (default true)"),
					queryParam("images", "Also fetch preview images (default true)"),
					queryParam("max_images", "Preview images to keep (default 4)")),
			},
			"/api/enrich": map[string]any{
				"get": operation("Progress of the running or most recent enrichment run",
					"Enrichment", jsonResponse(object(nil))),
				"post": operation(
					"Start a background enrichment sweep. scope=all covers the library; "+
						"scope=search takes the same filter parameters as /api/models and "+
						"covers every model matching them, not just the page on screen.",
					"Enrichment", jsonResponse(object(nil)),
					queryParam("scope", `"all" or "search" (default "all")`),
					queryParam("refresh", "Re-ask models that already have an archived response (default false)"),
					queryParam("images", "Also fetch preview images (default true)"),
					queryParam("max_images", "Preview images to keep per model (default 4)"),
					queryParam("limit", "Stop after this many models (0 for all)")),
			},
			"/api/enrich/{id}": map[string]any{
				"delete": operation(
					"Stop the running sweep. Everything already archived stays archived, "+
						"and re-running continues where this left off.",
					"Enrichment", emptyResponse(), pathParam("id", "Enrichment run id")),
			},
			"/api/remote-image": get(
				"Proxy a provider thumbnail. The page's CSP is img-src 'self', so a "+
					"remote URL in an <img> is refused outright -- and loading one "+
					"directly would disclose the viewer's address to a provider CDN on "+
					"every search. Host-checked, size-capped and content-sniffed, "+
					"because it is an outbound fetcher driven by a client-supplied URL.",
				"Browse",
				map[string]any{
					"200": map[string]any{
						"description": "Image bytes",
						"content": map[string]any{
							"image/*": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
						},
					},
					"403": jsonRef("Host not allowed", "Error"),
				},
				queryParam("url", "Image URL at a known provider host")),

			// --- downloads (Phase 6) ---------------------------------------
			"/api/downloads": map[string]any{
				"get": operation("Every tracked transfer, for progress polling", "Downloads",
					jsonResponse(array(object(nil)))),
				"post": operation(
					"Start a transfer. Refused in read-only mode, refused for a source "+
						"host that is not a known provider, and refused for a destination "+
						"that is not a managed root -- the destination is never inferred "+
						"from the URL or the filename. 409 with the running job's id when "+
						"the same transfer is already in flight.",
					"Downloads", jsonResponse(object(nil))),
			},
			"/api/downloads/{id}": map[string]any{
				"delete": operation(
					"Cancel a transfer in flight, or forget a terminal one. A partial "+
						"file is kept either way, so the same id resumes rather than "+
						"restarting.",
					"Downloads", jsonResponse(object(nil)), pathParam("id", "Download id")),
			},
			"/api/downloads/roots": get(
				"The directories a download may target. The client picks from this "+
					"list; it is not trusted to invent one.",
				"Downloads", jsonResponse(array(str("")))),

			"/api/models/{sha}/previews/generated": map[string]any{
				"post": operation(
					"Attach an image from the configured ComfyUI output folder, named "+
						"relative to it. Only that one directory is readable, and the path "+
						"has traversal stripped and containment re-checked after symlinks.",
					"Media", jsonResponse(object(nil)), pathParam("sha", "Model SHA256")),
			},
			"/api/models/{sha}/previews/order": map[string]any{
				"put": operation(
					"Set preview display order. Manual previews still sort ahead of "+
						"fetched ones -- order within a tier is yours, the tiering is not.",
					"Media", jsonResponse(object(nil)), pathParam("sha", "Model SHA256")),
			},

			"/api/comfy": get(
				"Whether a ComfyUI is configured and answering, plus the workflow "+
					"placeholders a template may use",
				"Media", jsonResponse(object(nil))),
			"/api/models/{sha}/previews/render": map[string]any{
				"post": operation(
					"Render a thumbnail with ComfyUI and attach it. Returns 202 with a "+
						"job; a render is tens of seconds of someone else's GPU, so it is "+
						"polled rather than awaited. The workflow must be ComfyUI's API "+
						"format -- the editor format is refused with an explanation. The "+
						"result is stored with a manual source, so enrichment cannot "+
						"displace it.",
					"Media", jsonResponse(object(nil)), pathParam("sha", "Model SHA256")),
			},
			"/api/comfy/workflows": get(
				"Saved workflows in the configured folder, each flagged for whether "+
					"it is the API format ComfyUI can queue", "Media", jsonResponse(object(nil))),
			"/api/comfy/status": get(
				"Per base-model family: which workflow is in use, where it came from, "+
					"and anything wrong with it", "Media", jsonResponse(object(nil))),
			"/api/comfy/adopt": map[string]any{
				"post": operation(
					"Extract the API-format graph a ComfyUI render carries in its PNG "+
						"metadata. Saves nothing -- the caller reviews it and chooses a "+
						"family to store it against.",
					"Media", jsonResponse(object(nil))),
			},
			"/api/models/{sha}/previews/render/plan": map[string]any{
				"post": operation(
					"Everything a render would do, minus the render: the workflow "+
						"chosen, every input that would be rewritten with its old and new "+
						"value, and any warnings. Contacts nothing.",
					"Media", jsonResponse(object(nil)), pathParam("sha", "Model SHA256")),
			},

			"/api/renders": get("Tracked renders, newest first", "Media",
				jsonResponse(object(nil))),
			"/api/renders/{id}": map[string]any{
				"delete": operation(
					"Stop waiting on a render. Whatever ComfyUI already queued stays "+
						"queued -- this app does not clear someone else's queue.",
					"Media", jsonResponse(object(nil)), pathParam("id", "Render id")),
			},

			"/api/models/{sha}/previews/{image}/workflow": get(
				"The ComfyUI workflow JSON this image carried, as a download", "Media",
				jsonResponse(object(nil)),
				pathParam("sha", "Model SHA256"), pathParam("image", "Image SHA256")),

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
