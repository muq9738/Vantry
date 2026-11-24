use serde::{Serialize, de::DeserializeOwned};
use serde_json::{Value, json};
use vantry_protocol::*;

fn roundtrip<T: Serialize + DeserializeOwned>(wire: &Value) {
    let value: T = serde_json::from_value(wire.clone()).unwrap();
    assert_eq!(
        serde_json::to_value(value).unwrap(),
        *wire,
        "{}",
        std::any::type_name::<T>()
    );
}
#[test]
fn reads_and_writes_v0142_go_encoder_records() {
    // encoding/json output from v0.1.42 with all exported wire fields populated.
    let doc: Value = serde_json::from_str(include_str!("fixtures/v1.json")).unwrap();
    roundtrip::<Vec<CommandKind>>(&doc["CommandKind"]);
    roundtrip::<Command>(&doc["api.Command"]);
    roundtrip::<Receipt>(&doc["api.Receipt"]);
    roundtrip::<Snapshot>(&doc["api.Snapshot"]);
    roundtrip::<CoreEvent>(&doc["api.Event"]);
    roundtrip::<Capabilities>(&doc["api.ServerStatus"]);
    roundtrip::<PiRuntimeStatus>(&doc["api.PiRuntimeStatus"]);
    roundtrip::<ApiError>(&doc["api.Error"]);
    roundtrip::<WorkbenchResponse>(&doc["api.WorkbenchResponse"]);
    roundtrip::<ProviderRequest>(&doc["provider.Request"]);
    roundtrip::<ProviderEvent>(&doc["provider.Event"]);
    roundtrip::<ControlResponse>(&doc["provider.Control"]);
    roundtrip::<ControlResult>(&doc["provider.ControlResult"]);
}
#[test]
fn omission_explicit_false_nil_slices_and_legacy_offsets() {
    roundtrip::<ReplyPart>(&json!({"reasoningDelta":"step"}));
    roundtrip::<ReplyPart>(&json!({"delta":"hello"}));
    roundtrip::<ResyncRequired>(&json!({"code":"resync_required","message":"behind"}));
    roundtrip::<ProviderEvent>(
        &json!({"occurredAt":"0001-01-01T00:00:00Z","sequence":1,"data":null}),
    );
    assert_eq!(
        serde_json::to_value(Thread::default()).unwrap()["createdAt"],
        "0001-01-01T00:00:00Z"
    );
    let mut cmd = Command::new("c", CommandKind::UpdateThread);
    cmd.pinned = Some(false);
    assert_eq!(
        serde_json::to_value(cmd).unwrap(),
        json!({"commandId":"c","version":1,"kind":"thread.update","pinned":false})
    );
    roundtrip::<Snapshot>(
        &json!({"sequence":0,"threads":null,"runs":[],"tasks":null,"cohorts":[]}),
    );
    let tool:ToolCall=serde_json::from_value(json!({"id":"t","threadId":"th","runId":"r","attemptId":"a","name":"read","status":"running","updatedAt":"2026-01-02T00:00:00Z"})).unwrap();
    assert_eq!(tool.output_offset, -1);
    assert_eq!(
        serde_json::to_value(ApiError {
            code: ErrorCode::ResyncRequired,
            message: "behind".into()
        })
        .unwrap(),
        json!({"code":"resync_required","message":"behind"})
    );
}
