//! Desktop preference shared with v0.1.42's desktop-state.json.
use serde::{Deserialize, Serialize};
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Language {
    En,
    Ja,
    #[default]
    #[serde(other)]
    Zh,
}
impl Language {
    pub const ALL: [Self; 3] = [Self::Zh, Self::En, Self::Ja];
    pub fn name(self) -> &'static str {
        match self {
            Self::Zh => "简体中文",
            Self::En => "English",
            Self::Ja => "日本語",
        }
    }
    pub fn label<'a>(self, zh: &'a str) -> &'a str {
        if self == Self::Zh {
            return zh;
        }
        let (en, ja) = match zh {
            "设置" => ("Settings", "設定"),
            "通用" => ("General", "一般"),
            "外观" => ("Appearance", "外観"),
            "Agent 与权限" => ("Agents & permissions", "エージェントと権限"),
            "提供方" => ("Providers", "プロバイダー"),
            "环境" => ("Environments", "環境"),
            "关于与更新" => ("About & updates", "情報と更新"),
            "语言与输入" => ("Language & input", "言語と入力"),
            "界面语言" => ("Interface language", "表示言語"),
            "发送快捷键" => ("Send shortcut", "送信ショートカット"),
            "Ctrl Enter 发送" => ("Ctrl Enter sends", "Ctrl Enter で送信"),
            "Enter 发送" => ("Enter sends", "Enter で送信"),
            "子任务" => ("Subtasks", "サブタスク"),
            "子任务自动续行" => ("Auto-continue subtasks", "サブタスクの自動続行"),
            "子任务全部完成后，父任务自动带着结果继续。" => (
                "When subtasks finish, the parent continues with their results.",
                "全サブタスクの完了後、親タスクが結果を受け取り続行します。",
            ),
            "主题风格" => ("Theme & appearance", "テーマと外観"),
            "明暗模式" => ("Color mode", "表示モード"),
            "跟随系统" => ("System", "システム"),
            "浅色" => ("Light", "ライト"),
            "深色" => ("Dark", "ダーク"),
            "新会话默认工具权限" => ("Default tool permissions", "既定のツール権限"),
            "工作区沙箱隔离" => ("Workspace sandbox", "ワークスペースのサンドボックス"),
            "Pi 执行器" => ("Pi runtime", "Pi ランタイム"),
            "远端互联与配对" => ("Remote access and pairing", "リモート接続とペアリング"),
            "应用更新" => ("Updates", "アップデート"),
            "用量统计" => ("Usage", "使用量"),
            "文件" => ("Files", "ファイル"),
            "终端" => ("Terminal", "ターミナル"),
            "插件" => ("Plugins", "プラグイン"),
            "预览" => ("Preview", "プレビュー"),
            "提交" => ("Commit", "コミット"),
            "日志" => ("Log", "ログ"),
            "比较" => ("Compare", "比較"),
            "关闭设置" => ("Close settings", "設定を閉じる"),
            "设置分区" => ("Settings sections", "設定セクション"),
            "新建会话 (⌘N)" => ("New thread (⌘N)", "新規スレッド (⌘N)"),
            "已归档" => ("Settled", "アーカイブ済み"),
            "搜索会话 (⌘K)" => ("Search threads (⌘K)", "スレッドを検索 (⌘K)"),
            "选择模型" => ("Choose model", "モデルを選択"),
            "发送" => ("Send", "送信"),
            "新会话" => ("New thread", "新規スレッド"),
            "配色预设" => ("Theme preset", "テーマプリセット"),
            "用户扩展" => ("User extensions", "ユーザー拡張機能"),
            "用户 MCP" => ("User MCP", "ユーザー MCP"),
            "检查更新" => ("Check for updates", "アップデートを確認"),
            "立即下载" => ("Download update", "アップデートをダウンロード"),
            "安装并重启" => ("Install & restart", "インストールして再起動"),
            "新建配对码" => ("Create pairing code", "ペアリングコードを作成"),
            "已配对设备" => ("Paired devices", "ペアリング済みデバイス"),
            "添加环境" => ("Add environment", "環境を追加"),
            "项目管理" => ("Projects", "プロジェクト"),
            "抓取" => ("Fetch", "フェッチ"),
            "拉取" => ("Pull", "プル"),
            "推送" => ("Push", "プッシュ"),
            "本地分支" => ("Local branches", "ローカルブランチ"),
            "远程分支" => ("Remote branches", "リモートブランチ"),
            "交换方向" => ("Swap direction", "方向を入れ替え"),
            "文件变更" => ("File changes", "ファイル変更"),
            "展开 Git 面板" => ("Expand Git panel", "Git パネルを展開"),
            "在内置终端中运行" => ("Run in built-in terminal", "内蔵ターミナルで実行"),
            "标记已完成" => ("Mark completed", "完了としてマーク"),
            "取消请求" => ("Cancel request", "リクエストをキャンセル"),
            "提交选择" => ("Submit choice", "選択を送信"),
            "确认并派工" => ("Confirm & dispatch", "確認してディスパッチ"),
            "直接在本会话执行" => ("Run directly here", "このスレッドで直接実行"),
            "复制" => ("Copy", "コピー"),
            "关闭" => ("Close", "閉じる"),
            "刷新" => ("Refresh", "更新"),
            _ => return zh,
        };
        if self == Self::En { en } else { ja }
    }
}
