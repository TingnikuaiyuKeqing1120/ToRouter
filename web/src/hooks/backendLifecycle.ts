import { useCallback, useEffect, useRef, useState } from 'react';

interface Ready {service:string;installation_id:string;instance_id:string;port:number;version?:string}
interface Lifecycle extends Ready {state:'running'|'stopped'|'restarting'|'updating';target_port?:number}
export type BackendState = 'running'|'restarting'|'updating'|'stopped';
class UnverifiedGateway extends Error {}

export function useBackendLifecycle(enabled:boolean,onReconnect:(origin:string,ready?:Ready,updated?:boolean)=>void) {
  const [state,setState]=useState<BackendState>('running');
  const [version,setVersion]=useState(0);
  const expectedUpdate=useRef(false);
  const expectUpdate=useCallback((active:boolean)=>{expectedUpdate.current=active;},[]);
  useEffect(()=>{
    if(!enabled)return;
    let disposed=false,origin=window.location.origin,identity='',instance='',port=0;
    let recoveryAt=0,updating=false,observedUpdate=false;
    const abort=new AbortController();
    const pause=()=>new Promise<void>(resolve=>{
      const done=()=>{clearTimeout(timer);abort.signal.removeEventListener('abort',done);resolve();};
      const timer=setTimeout(done,500);abort.signal.addEventListener('abort',done,{once:true});
      if(abort.signal.aborted)done();
    });
    const read=async(watch:boolean):Promise<Ready|Lifecycle>=>{
      const request=new AbortController();const parentAbort=()=>request.abort();abort.signal.addEventListener('abort',parentAbort,{once:true});
      const timeout=setTimeout(()=>request.abort(),watch?30000:1500);
      try {
      const response=await fetch(`${origin}/_console/ready${watch?'?watch=1':''}`,{credentials:'omit',cache:'no-store',redirect:'error',signal:request.signal});
      if(!response.ok)throw new Error('gateway unavailable');
      let value:Ready|Lifecycle;
      try {value=await response.json() as Ready|Lifecycle;}
      catch(error){
        if(request.signal.aborted)throw new Error('gateway timeout');
        if(error instanceof SyntaxError)throw new UnverifiedGateway('invalid gateway');
        throw error;
      }
      if(!value||value.service!=='to-router'||!value.installation_id||!value.instance_id||value.port!==Number(new URL(origin).port||80))throw new UnverifiedGateway('invalid gateway');
      if(identity&&value.installation_id!==identity)throw new Error('different installation');
      if(request.signal.aborted)throw new Error('gateway timeout');
      return value;
      }finally{clearTimeout(timeout);abort.signal.removeEventListener('abort',parentAbort);}
    };
    const run=async()=>{
      const initialAt=Date.now();
      while(!disposed){
        try {const initial=await read(false);if(disposed)return;identity=initial.installation_id;instance=initial.instance_id;port=initial.port;setState('running');break;}
        catch(error){
          if(disposed)return;
          // A foreign page is not a gateway; a connection failure may be transient.
          if(error instanceof UnverifiedGateway){if(version>0)setState('stopped');return;}
          if(Date.now()-initialAt>=30000){setState('stopped');return;}
          setState('restarting');await pause();
        }
      }
      while(!disposed){
        try {
          const next=await read(true) as Lifecycle;
          if(disposed)return;
          if(next.state==='running' && next.instance_id!==instance && (updating||expectedUpdate.current)){
            updating=false;observedUpdate=false;expectedUpdate.current=false;instance=next.instance_id;setState('running');onReconnect(origin,next,true);
          }
          if(next.state==='stopped'&&!updating&&!expectedUpdate.current){setState('stopped');return;}
          if(next.state==='running'){instance=next.instance_id;recoveryAt=0;setState('running');continue;}
          if(next.state!=='restarting'&&next.state!=='updating'&&next.state!=='stopped')return;
          observedUpdate=observedUpdate||next.state==='updating';
          updating=observedUpdate||expectedUpdate.current;
          const target=next.target_port??(updating?next.port:undefined);
          if(!Number.isInteger(target)||target!<1||target!>65535)return;
          port=target!;
          origin=new URL(origin).protocol+'//'+new URL(origin).hostname+':'+port;
          setState(updating?'updating':'restarting');
        }catch{if(disposed)return;updating=observedUpdate||expectedUpdate.current;setState(updating?'updating':'restarting');}
        if(!recoveryAt)recoveryAt=Date.now();
        while(!disposed){
          if(updating&&!observedUpdate&&!expectedUpdate.current){updating=false;recoveryAt=Date.now();setState('restarting');}
          if(Date.now()-recoveryAt>=(updating?90000:30000)){setState('stopped');return;}
          try {
            const ready=await read(false);
            if(disposed)return;
            // A running successor is confirmed before starting another watch;
            // the heartbeat wait must not consume the reconnect grace.
            if(ready.installation_id===identity && (!updating||ready.instance_id!==instance) && (!('state' in ready)||(ready as Lifecycle).state==='running')){
              recoveryAt=0;instance=ready.instance_id;setState('running');
              if(updating){updating=false;observedUpdate=false;expectedUpdate.current=false;onReconnect(origin,ready,true);}else onReconnect(origin);
              break;
            }
          }catch{ /* Wait for the same installation, never auto-start a daemon. */ }
          await pause();
        }
      }
    };
    void run();
    return()=>{disposed=true;abort.abort();};
  },[enabled,version,onReconnect]);
  return {state,expectUpdate,retry:()=>{setState('restarting');setVersion(value=>value+1);}};
}
