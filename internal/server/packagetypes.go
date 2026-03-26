package server

// PackageRequest is the body of POST /api/v1/packages.
type PackageRequest struct {
	Name             string   `json:"name"`              // package name or pkgdepends ref
	LoadedNamespaces []string `json:"loaded_namespaces"` // from loadedNamespaces() in R
}

// PackageResponse is returned by POST /api/v1/packages.
type PackageResponse struct {
	Status       string `json:"status"`                  // "ok", "transfer", "error"
	Message      string `json:"message,omitempty"`
	TransferPath string `json:"transfer_path,omitempty"` // set when status == "transfer"
}
