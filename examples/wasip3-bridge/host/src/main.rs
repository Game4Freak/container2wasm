//! wasmtime host for the wasip3-bridge example.
//!
//! usage: wasip3-bridge-host <out.wasm> [guest command...]
//!
//! Provides `example:greeter/host`, runs the container and, once the guest
//! program serves `example:greeter/guest`, calls `handle` on it.
use anyhow::{Context, Result};
use wasmtime::component::{Component, Linker, ResourceTable};
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

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<()> {
    let mut args = std::env::args().skip(1);
    let path = args.next().context("usage: wasip3-bridge-host <out.wasm> [guest command...]")?;
    let guest_args: Vec<String> = args.collect();

    let mut config = Config::new();
    config.wasm_component_model(true);
    config.wasm_component_model_async(true);
    config.async_support(true);
    let engine = Engine::new(&config)?;
    let component = Component::from_file(&engine, &path)?;

    let mut linker = Linker::<State>::new(&engine);
    wasmtime_wasi::p3::add_to_linker(&mut linker)?;

    // example:greeter/host
    let mut host = linker.instance("example:greeter/host@0.1.0")?;
    host.func_wrap_concurrent("greet", |_accessor, (name,): (String,)| {
        Box::pin(async move { Ok((format!("hello, {name}!"),)) })
    })?;
    host.func_wrap("add", |_store, (a, b): (u32, u32)| Ok((a + b,)))?;

    let mut wasi = WasiCtxBuilder::new();
    wasi.inherit_stdio().arg("out.wasm").args(&guest_args);
    let mut store = Store::new(&engine, State { ctx: wasi.build(), table: ResourceTable::new() });
    let instance = linker.instantiate_async(&mut store, &component).await?;

    let run_iface = instance.get_export_index(&mut store, None, "wasi:cli/run@0.3.0").context("wasi:cli/run")?;
    let run_idx = instance.get_export_index(&mut store, Some(&run_iface), "run").context("run")?;
    let run = instance.get_typed_func::<(), (Result<(), ()>,)>(&mut store, &run_idx)?;

    let guest_iface = instance.get_export_index(&mut store, None, "example:greeter/guest@0.1.0").context("guest")?;
    let handle_idx = instance.get_export_index(&mut store, Some(&guest_iface), "handle").context("handle")?;
    let handle = instance.get_typed_func::<(String,), (String,)>(&mut store, &handle_idx)?;

    store
        .run_concurrent(async |accessor| -> Result<()> {
            let run_fut = async {
                let (r,) = run.call_concurrent(accessor, ()).await?;
                Ok::<_, anyhow::Error>(r)
            };
            let calls = async {
                // Give the guest time to boot and start serving.
                tokio::time::sleep(std::time::Duration::from_secs(60)).await;
                let (reply,) = handle.call_concurrent(accessor, ("ping from host".to_string(),)).await?;
                println!("host: guest replied {reply:?}");
                Ok::<_, anyhow::Error>(())
            };
            let (r, c) = futures::join!(run_fut, calls);
            c?;
            anyhow::ensure!(r?.is_ok(), "container exited with an error");
            Ok(())
        })
        .await??;
    Ok(())
}
