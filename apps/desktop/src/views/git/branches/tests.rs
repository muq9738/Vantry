use super::*;

#[test]
fn branch_tree_filters_paths_and_actions_and_opens_matching_folders() {
    let c = Catalog {
        current: "main".into(),
        branches: [
            ("main", false),
            ("topic/ui/modal", false),
            ("topic/api", false),
            ("team/topic/ui", true),
        ]
        .into_iter()
        .map(|(name, remote)| Branch {
            name: name.into(),
            remote,
            ..Default::default()
        })
        .collect(),
        recent: vec!["topic/ui/modal".into(), "main".into()],
        ..Default::default()
    };
    let initial = rows(&c, "", "", &BTreeSet::new(), false);
    assert!(initial.contains(&Row::Folder(
        "topic".into(),
        "本地分支/topic".into(),
        0,
        false
    )));
    assert!(initial.contains(&Row::Group("最近使用".into())));
    assert!(
        !initial
            .iter()
            .any(|r| matches!(r,Row::Branch(b,_) if b.name=="topic/ui/modal"))
    );
    let open = ["本地分支/topic".into(), "本地分支/topic/ui".into()]
        .into_iter()
        .collect();
    assert!(rows(&c, "", "", &open, false).contains(&Row::Branch(c.branches[1].clone(), 2)));
    let results = rows(&c, "", "UI", &Default::default(), false);
    assert!(results.contains(&Row::Branch(c.branches[3].clone(), 2)));
    assert!(
        !results
            .iter()
            .any(|r| matches!(r,Row::Branch(b,_) if b.name=="topic/api"))
    );
    assert!(
        matches!(rows(&c,"","New Branch",&Default::default(),false).as_slice(),[Row::Action(a)] if a.op=="create")
    );
    let current = rows(&c, "refs/heads/main", "", &Default::default(), false);
    assert!(
        !current.iter().any(
            |r| matches!(r,Row::Action(a) if matches!(a.op,"delete"|"checkout"|"merge"|"push"))
        )
    );
    let remote = rows(
        &c,
        "refs/remotes/team/topic/ui",
        "",
        &Default::default(),
        false,
    );
    assert!(
        !remote.iter().any(
            |r| matches!(r,Row::Action(a) if matches!(a.op,"delete"|"rename"|"push"|"update"))
        )
    );
}
