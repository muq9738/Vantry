use std::{
    fs::OpenOptions,
    io::Write,
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    sync::Arc,
    time::{Duration, Instant},
};
use tokio_util::sync::CancellationToken;
use vantry_core::{
    Core, DurableProvider,
    runner::{Emit, ProviderResult, StartMode},
};
use vantry_protocol::{
    Command as CoreCommand, CommandKind, ControlResponse, ProviderEvent, ProviderRequest, RunStatus,
};
struct Journal(PathBuf);
#[async_trait::async_trait]
impl DurableProvider for Journal {
    async fn run_events(&self, r: ProviderRequest, mode: StartMode, emit: Emit) -> ProviderResult {
        if mode == StartMode::Submit {
            assert_eq!(r.message, "one initial prompt");
            let mut submissions = OpenOptions::new()
                .append(true)
                .create(true)
                .open(self.0.join("prompts"))
                .unwrap();
            writeln!(submissions, "{}", r.attempt_id).unwrap();
            submissions.sync_all().unwrap();
            std::fs::write(self.0.join("submitted"), &r.attempt_id).unwrap();
            std::future::pending().await
        } else {
            assert!(r.message.is_empty());
            assert_eq!(r.after_sequence, 0);
            assert_eq!(
                std::fs::read_to_string(self.0.join("submitted")).unwrap(),
                r.attempt_id
            );
            emit(ProviderEvent {
                sequence: 1,
                delta: "recovered".into(),
                ..Default::default()
            })
            .unwrap();
            Ok(())
        }
    }
    async fn recoverable(&self, _: &ProviderRequest) -> bool {
        self.0.join("submitted").is_file()
    }
    async fn stop(&self, _: &ProviderRequest) -> ProviderResult {
        panic!("Core death must not stop execution")
    }
    async fn control(&self, _: &ProviderRequest, _: &ControlResponse) -> ProviderResult {
        unreachable!()
    }
}
struct OwnedChild(Child);
impl Drop for OwnedChild {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}
#[tokio::test]
async fn sigkill_before_first_event_does_not_submit_again() {
    const CHILD: &str = "VANTRY_TEST_KILL_CHILD_DIR";
    if let Some(path) = std::env::var_os(CHILD) {
        let path = Path::new(&path);
        let core = Core::open(path).unwrap();
        let r = core
            .dispatch(&CoreCommand::new("thread", CommandKind::CreateThread))
            .unwrap();
        core.dispatch(&CoreCommand {
            thread_id: r.thread_id,
            message: "one initial prompt".into(),
            ..CoreCommand::new("send", CommandKind::SendMessage)
        })
        .unwrap();
        core.run_scheduler(Arc::new(Journal(path.into())), CancellationToken::new())
            .await
            .unwrap();
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let mut process = OwnedChild(
        Command::new(std::env::current_exe().unwrap())
            .args([
                "--exact",
                "sigkill_before_first_event_does_not_submit_again",
            ])
            .env(CHILD, dir.path())
            .stdout(Stdio::null())
            .spawn()
            .unwrap(),
    );
    let deadline = Instant::now() + Duration::from_secs(5);
    while !dir.path().join("submitted").is_file() {
        assert!(Instant::now() < deadline);
        assert!(process.0.try_wait().unwrap().is_none());
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert!(Core::open(dir.path()).is_err());
    process.0.kill().unwrap();
    assert!(!process.0.wait().unwrap().success());
    drop(process);
    let core = Core::open(dir.path()).unwrap();
    let token = CancellationToken::new();
    let worker = tokio::spawn({
        let core = core.clone();
        let p = Journal(dir.path().into());
        let token = token.clone();
        async move { core.run_scheduler(Arc::new(p), token).await }
    });
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let runs = core.snapshot().unwrap().runs.unwrap();
            if runs[0].status == RunStatus::Completed {
                assert_eq!(runs[0].output, "recovered");
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .unwrap();
    assert_eq!(
        std::fs::read_to_string(dir.path().join("prompts"))
            .unwrap()
            .lines()
            .count(),
        1
    );
    token.cancel();
    worker.await.unwrap().unwrap();
}
