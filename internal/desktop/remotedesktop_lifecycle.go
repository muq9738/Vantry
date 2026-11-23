package desktop

// initializeRemoteDesktop creates only the local viewer/decoder adapter. It
// never starts capture, hosting, a relay or an input injector.
func (w *workbench) initializeRemoteDesktop() {
	if w.defaults.RemoteDesktop == nil && w.defaults.RemoteDesktopFactory != nil {
		w.defaults.RemoteDesktop = w.defaults.RemoteDesktopFactory(w.ctx)
	}
}

func (w *workbench) releaseRemoteDesktop() {
	w.cancelRemoteHostingIntent()
	w.disconnectRemoteDesktop()
	driver := w.defaults.RemoteDesktop
	w.defaults.RemoteDesktop = nil
	if closer, ok := driver.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}
