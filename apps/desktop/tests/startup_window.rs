use nsh_core::{
    ApproxTextEngine, Command, Core, EventKind, EventPayload, MountItem, NodeId,
    desktop::{DesktopCommand, WindowCloseCause, WindowEvent, WindowRequest},
    element::{Environment, Root},
    platform::{Idiom, Os},
    prelude::{Size, px},
};
use vantry_desktop::{App, bridge::Endpoint, cli::LaunchConfig, set_launch_config};

fn window_commands(ui: &mut Core<ApproxTextEngine>) -> Vec<(NodeId, DesktopCommand)> {
    ui.commit()
        .into_iter()
        .flat_map(|batch| batch.items)
        .filter_map(|item| match item {
            MountItem::Command {
                id,
                command: Command::Desktop(command),
            } => Some((id, *command)),
            _ => None,
        })
        .filter(|(_, command)| {
            matches!(
                command,
                DesktopCommand::OpenWindow(_) | DesktopCommand::Request(_)
            )
        })
        .collect()
}

#[test]
fn launch_and_reopen_show_one_workbench_window() {
    let dir = tempfile::tempdir().unwrap();
    let mut config = LaunchConfig::default_for(dir.path().to_owned());
    config.endpoint = Endpoint::Mock;
    assert!(set_launch_config(config));

    let mut ui = Core::new(ApproxTextEngine, Size::new(px(800.0), px(600.0)));
    ui.set_environment(Environment {
        os: Os::MacOs,
        idiom: Idiom::Desktop,
        ..Default::default()
    });
    ui.mount_root(&Root::of::<App>());
    let launch = window_commands(&mut ui);
    let [(id, DesktopCommand::OpenWindow(window))] = launch.as_slice() else {
        panic!("expected one workbench window at launch, got {launch:?}");
    };
    assert_eq!(window.options.attributes.title.as_deref(), Some("Vantry"));

    ui.reopened();
    assert_eq!(
        window_commands(&mut ui),
        [(*id, DesktopCommand::Request(WindowRequest::Focus))]
    );

    ui.component_event(
        *id,
        EventKind::Window,
        &EventPayload::Window(Box::new(WindowEvent::Closed {
            cause: WindowCloseCause::ClosePress,
        })),
    );
    let _ = ui.commit();
    ui.reopened();
    let reopened = window_commands(&mut ui);
    assert!(
        matches!(reopened.as_slice(), [(new_id, DesktopCommand::OpenWindow(new_window))]
        if new_id != id && new_window.options.id == window.options.id)
    );
}
