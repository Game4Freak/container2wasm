use anyhow::{Context, Result};
use wasmtime::component::{Component, ComponentType, Lift, Linker, Lower, ResourceTable};
use wasmtime::{Config, Engine, Store};
use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView};

struct State {
    ctx: WasiCtx,
    table: ResourceTable,
}

impl WasiView for State {
    fn ctx(&mut self) -> WasiCtxView<'_> {
        WasiCtxView { ctx: &mut self.ctx, table: &mut self.table }
    }
}

#[derive(ComponentType, Lift, Lower, Clone, Copy, Debug)]
#[component(record)]
struct Point {
    x: i32,
    y: i32,
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<()> {
    let path = std::env::args().nth(1).context("usage: testhost <composed.wasm>")?;
    let mut config = Config::new();
    config.wasm_component_model(true);
    config.wasm_component_model_async(true);
    config.async_support(true);
    let engine = Engine::new(&config)?;
    let component = Component::from_file(&engine, &path)?;

    let mut linker = Linker::<State>::new(&engine);
    wasmtime_wasi::p3::add_to_linker(&mut linker)?;
    let mut host = linker.instance("c2w:test/host@0.1.0")?;
    host.func_wrap("add", |_store, (a, b): (u32, u32)| {
        println!("host: add({a}, {b})");
        Ok((a + b,))
    })?;
    host.func_wrap_concurrent("greet", |_accessor, (name,): (String,)| {
        Box::pin(async move {
            println!("host: greet({name:?})");
            Ok((format!("hello, {name}!"),))
        })
    })?;
    host.func_wrap("swap", |_store, (p,): (Point,)| {
        println!("host: swap({p:?})");
        Ok((Point { x: p.y, y: p.x },))
    })?;

    let ctx = WasiCtxBuilder::new().inherit_stdio().build();
    let mut store = Store::new(&engine, State { ctx, table: ResourceTable::new() });
    let instance = linker.instantiate_async(&mut store, &component).await?;

    let run_iface = instance
        .get_export_index(&mut store, None, "wasi:cli/run@0.3.0")
        .context("wasi:cli/run export")?;
    let run_idx = instance
        .get_export_index(&mut store, Some(&run_iface), "run")
        .context("run export")?;
    let run = instance.get_typed_func::<(), (Result<(), ()>,)>(&mut store, &run_idx)?;

    let guest_iface = instance
        .get_export_index(&mut store, None, "c2w:test/guest@0.1.0")
        .context("c2w:test/guest export")?;
    let echo_idx = instance.get_export_index(&mut store, Some(&guest_iface), "echo").context("echo")?;
    let sum_idx = instance.get_export_index(&mut store, Some(&guest_iface), "sum-list").context("sum-list")?;
    let echo = instance.get_typed_func::<(String,), (String,)>(&mut store, &echo_idx)?;
    let sum = instance.get_typed_func::<(Vec<u32>,), (u32,)>(&mut store, &sum_idx)?;

    let result = store
        .run_concurrent(async |accessor| -> Result<()> {
            let run_fut = async {
                let (r,) = run.call_concurrent(accessor, ()).await?;
                println!("host: run returned {r:?}");
                Ok::<_, anyhow::Error>(r)
            };
            let calls = async {
                let (e,) = echo.call_concurrent(accessor, ("hello from host".to_string(),)).await?;
                println!("host: echo -> {e:?}");
                let (s,) = sum.call_concurrent(accessor, (vec![1, 2, 3, 4],)).await?;
                println!("host: sum-list -> {s}");
                Ok::<_, anyhow::Error>((e, s))
            };
            let (r, c) = futures::join!(run_fut, calls);
            let r = r?;
            let (e, s) = c?;
            anyhow::ensure!(r.is_ok(), "vm exited with an error");
            anyhow::ensure!(e == "hello from host", "unexpected echo result {e:?}");
            anyhow::ensure!(s == 10, "unexpected sum result {s}");
            Ok(())
        })
        .await;
    result??;
    println!("host: all checks passed");
    Ok(())
}
