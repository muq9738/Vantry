use nsh_base::theme;
use nsh_core::prelude::*;

#[component]
pub fn App() -> impl IntoElement {
    view()
        .flex_1()
        .bg(move || theme().background)
        .child(text("Vantry (NSH Rust Skeleton)"))
}
