package gui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/wan"
)

// wanController belongs to the process actually serving the gateway. A GUI
// observing another process must not create a second public listener.
type wanController interface {
	WANStatus() wan.Status
	StartWAN() error
	StopWAN() error
	StartWANAuth(bool) (wan.AuthStatus, error)
	CancelWANAuth(uint64) error
	LogoutWAN() error
	InstallWANRuntime() error
}

type wanJSON struct {
	wan.Status
	Controllable bool         `json:"controllable"`
	Keys         []access.Key `json:"keys"`
	KeysError    string       `json:"keysError,omitempty"`
	Secret       string       `json:"secret,omitempty"`
}

func wanOwner() wanController {
	if owner := served.Load(); owner != nil {
		return owner
	}
	return nil
}

// wanRoutes resolves ownership for each request, not from Handler's original
// gateway pointer, which can be stale after a restart or a handover.
func wanRoutes(mux *http.ServeMux, owner func() wanController) {
	respond := func(w http.ResponseWriter, controller wanController, secret string) {
		keys, err := access.ListWAN()
		out := wanJSON{Status: wan.Status{State: "unknown"}, Keys: keys, Secret: secret}
		if err != nil {
			// A broken key file must not hide the owner's Stop and sign-out
			// controls. Unknown keys remain unavailable, never an empty list.
			out.Keys, out.KeysError = nil, "WAN keys could not be read; key changes are unavailable"
		}
		if controller != nil {
			out.Status, out.Controllable = controller.WANStatus(), true
		}
		writeJSON(w, out)
	}
	control := func(w http.ResponseWriter) wanController {
		controller := owner()
		if controller == nil {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]string{"error": "This window does not own the gateway. WAN controls are unavailable."})
		}
		return controller
	}
	mux.HandleFunc("GET /api/wan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		respond(w, owner(), "")
	})
	for _, action := range []string{"start", "stop"} {
		mux.HandleFunc("POST /api/wan/"+action, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			controller := control(w)
			if controller == nil {
				return
			}
			var err error
			if action == "start" {
				err = controller.StartWAN()
			} else {
				err = controller.StopWAN()
			}
			if err != nil {
				fail(w, err)
				return
			}
			respond(w, controller, "")
		})
	}
	mux.HandleFunc("POST /api/wan/auth/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		controller := control(w)
		if controller == nil {
			return
		}
		var in struct {
			Remember bool `json:"remember"`
		}
		if err := wanInput(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		if _, err := controller.StartWANAuth(in.Remember); err != nil {
			fail(w, err)
			return
		}
		respond(w, controller, "")
	})
	mux.HandleFunc("POST /api/wan/auth/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		controller := control(w)
		if controller == nil {
			return
		}
		var in struct {
			AttemptID uint64 `json:"attemptID"`
		}
		if err := wanInput(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		if in.AttemptID == 0 {
			fail(w, errors.New("WAN sign-in attempt is required"))
			return
		}
		if err := controller.CancelWANAuth(in.AttemptID); err != nil {
			fail(w, err)
			return
		}
		respond(w, controller, "")
	})
	for _, action := range []string{"auth/logout", "runtime/install"} {
		mux.HandleFunc("POST /api/wan/"+action, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			controller := control(w)
			if controller == nil {
				return
			}
			var in struct{}
			if err := wanInput(w, r, &in); err != nil {
				fail(w, err)
				return
			}
			var err error
			if action == "auth/logout" {
				err = controller.LogoutWAN()
			} else {
				err = controller.InstallWANRuntime()
			}
			if err != nil {
				fail(w, err)
				return
			}
			respond(w, controller, "")
		})
	}
	mux.HandleFunc("POST /api/wan/keys/{action}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		controller := control(w)
		if controller == nil {
			return
		}
		var in access.Change
		if err := wanInput(w, r, &in); err != nil {
			fail(w, err)
			return
		}
		secret, err := access.UpdateWAN(r.PathValue("action"), in)
		if err != nil {
			fail(w, err)
			return
		}
		respond(w, controller, secret)
	})
}

func wanInput(w http.ResponseWriter, r *http.Request, out any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return errors.New("WAN request is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("WAN request is invalid")
	}
	return nil
}
